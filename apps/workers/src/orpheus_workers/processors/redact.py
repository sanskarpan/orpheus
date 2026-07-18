"""text.redact processor + PII redaction wiring (PRD 08).

Standalone redaction over a transcript, and a shared helper the transcribe /
translate / summarize processors call so PII is masked before any downstream
step (or external LLM) sees the text. When keep_mapping is set, the
original↔masked mapping is written as a separate artifact flagged
sensitivity='pii_mapping' (fetching it requires the pii:unmask scope).
"""

from __future__ import annotations

import hashlib
import json
from typing import Any

from ..redact import redact_transcript
from . import register_processor
from .text_ops import _load_transcript, _params


@register_processor(
    "text.redact",
    display_name="Redact PII",
    description="Mask configurable PII entity types in a transcript.",
    tier="cpu_small",
    timeout_seconds=300,
    cost_per_job_usd=0.001,
    model_id="orpheus-redact",
    model_version_id="orpheus-redact-1",
)
async def redact_proc(ctx: dict[str, Any], job_id: str) -> dict[str, Any]:
    db = ctx["db"]
    job = db.fetchrow("SELECT org_id, artifact_id, params FROM jobs WHERE id = %s", job_id)
    if job is None:
        raise ValueError(f"job {job_id} not found")
    params = _params(job)
    transcript = _load_transcript(ctx, job, params)

    entities = params.get("entities")
    mask = params.get("mask", "type")
    redacted, summary, mapping = redact_transcript(transcript, entities=entities, mask=mask)

    result: dict[str, Any] = {
        "text": redacted.get("text", ""),
        "segments": redacted.get("segments", []),
        "language": redacted.get("language"),
        "redactions": summary,
        "model_version_id": "orpheus-redact-1",
    }

    if params.get("keep_mapping") and mapping:
        artifact_id = _write_mapping_artifact(ctx, job["org_id"], job_id, mapping)
        result["mapping_artifact_id"] = artifact_id
    return result


def _write_mapping_artifact(ctx: dict[str, Any], org_id: str, job_id: str, mapping: dict) -> str:
    """Store the un-redact mapping as a pii_mapping-sensitivity artifact.

    v1 stores the mapping JSON directly under a tenant-scoped key; the
    sensitivity flag forces the pii:unmask scope + shorter retention. A KMS
    envelope encrypt is a documented fast-follow (the interface is the same).
    """
    s3 = ctx["s3"]
    bucket = ctx["bucket"]
    data = json.dumps(mapping).encode("utf-8")
    key = f"pii-mappings/{org_id}/{job_id}.json"
    import tempfile
    from pathlib import Path

    with tempfile.NamedTemporaryFile(suffix=".json", delete=False) as tf:
        tmp = tf.name
    try:
        Path(tmp).write_bytes(data)
        s3.upload_file(bucket, key, tmp, "application/json")
    finally:
        Path(tmp).unlink(missing_ok=True)
    sha = hashlib.sha256(data).hexdigest()
    row = ctx["db"].fetchrow(
        """
        INSERT INTO artifacts (org_id, s3_bucket, s3_key, sha256, size_bytes, content_type, probe_status, sensitivity)
        VALUES (%s,%s,%s,%s,%s,'application/json','completed'::probe_status,'pii_mapping')
        RETURNING id::text
        """,
        org_id,
        bucket,
        key,
        sha,
        len(data),
    )
    return row["id"]
