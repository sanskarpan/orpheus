"""export.bundle processor — zip a bundle's artifacts into one S3 object (PRD 02).

Reads the bundle's items (resolved + RLS-checked at create time in the API),
downloads each artifact from S3, writes a deterministic zip (plus any embedded
job result.json docs), uploads it, and flips the bundle row to `ready` with
its size/key. On failure it marks the bundle `failed` and emits `bundle.failed`
before re-raising so the job's own retry/dead-letter path still applies.
"""

from __future__ import annotations

import json
import zipfile
from pathlib import Path
from typing import Any

from . import register_processor


def _params(job: dict) -> dict:
    params = job["params"] or {}
    if isinstance(params, str):
        params = json.loads(params)
    return params


@register_processor("export.bundle")
async def export_bundle(ctx: dict[str, Any], job_id: str) -> dict[str, Any]:
    db = ctx["db"]
    s3 = ctx["s3"]
    bucket = ctx["bucket"]
    work_dir = ctx["work_dir"]

    job = db.fetchrow("SELECT org_id, params FROM jobs WHERE id = %s", job_id)
    if job is None:
        raise ValueError(f"job {job_id} not found")
    org_id = job["org_id"]
    bundle_id = _params(job).get("bundle_id")
    if not bundle_id:
        raise ValueError("export.bundle job missing params.bundle_id")

    bundle = db.fetchrow("SELECT id, org_id, result_docs FROM bundles WHERE id = %s", bundle_id)
    if bundle is None:
        raise ValueError(f"bundle {bundle_id} not found")

    try:
        items = db.fetchall(
            "SELECT artifact_id, path_in_zip FROM bundle_items WHERE bundle_id = %s ORDER BY path_in_zip",
            bundle_id,
        )
        result_docs = bundle["result_docs"] or {}
        if isinstance(result_docs, str):
            result_docs = json.loads(result_docs)

        work = Path(work_dir)
        work.mkdir(parents=True, exist_ok=True)
        zip_path = work / f"bundle-{bundle_id}.zip"

        used_paths: set[str] = set()

        def unique_path(p: str) -> str:
            # Guard against duplicate arcnames (server-derived paths, so this is
            # belt-and-suspenders): suffix -1, -2, ... on collision.
            if p not in used_paths:
                used_paths.add(p)
                return p
            stem, dot, ext = p.rpartition(".")
            base = stem if dot else p
            suffix = ext if dot else ""
            i = 1
            while True:
                cand = f"{base}-{i}" + (f".{suffix}" if suffix else "")
                if cand not in used_paths:
                    used_paths.add(cand)
                    return cand
                i += 1

        count = 0
        with zipfile.ZipFile(zip_path, "w", zipfile.ZIP_DEFLATED) as zf:
            for item in items:
                art = db.fetchrow(
                    "SELECT s3_bucket, s3_key FROM artifacts WHERE id = %s", item["artifact_id"]
                )
                if art is None:
                    continue
                suffix = Path(art["s3_key"]).suffix or ".bin"
                local = work / f"bundle-{bundle_id}-{count}{suffix}"
                s3.download_file(art["s3_bucket"], art["s3_key"], str(local))
                zf.write(local, arcname=unique_path(item["path_in_zip"]))
                local.unlink(missing_ok=True)
                count += 1
            for path, doc in result_docs.items():
                zf.writestr(unique_path(path), json.dumps(doc, indent=2))
                count += 1

        s3_key = f"bundles/{org_id}/{bundle_id}.zip"
        size = s3.upload_file(bucket, s3_key, str(zip_path), "application/zip")
        zip_path.unlink(missing_ok=True)

        db.execute(
            """
            UPDATE bundles
            SET status = 'ready', s3_bucket = %s, s3_key = %s,
                size_bytes = %s, artifact_count = %s, updated_at = now()
            WHERE id = %s
            """,
            bucket,
            s3_key,
            size,
            count,
            bundle_id,
        )
        db.enqueue_outbox(
            org_id=org_id,
            aggregate_id=bundle_id,
            event_type="bundle.ready",
            payload={"bundle_id": bundle_id, "size_bytes": size, "artifact_count": count},
        )
        return {"bundle_id": bundle_id, "size_bytes": size, "artifact_count": count}
    except Exception as exc:
        db.execute(
            "UPDATE bundles SET status = 'failed', error = %s, updated_at = now() WHERE id = %s",
            str(exc)[:500],
            bundle_id,
        )
        db.enqueue_outbox(
            org_id=org_id,
            aggregate_id=bundle_id,
            event_type="bundle.failed",
            payload={"bundle_id": bundle_id, "error": str(exc)[:500]},
        )
        raise
