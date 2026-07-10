from __future__ import annotations

import os
from pathlib import Path
from typing import Any

import structlog
from mutagen import File as MutagenFile

from . import register_processor

logger = structlog.get_logger(__name__)


async def _download_artifact(
    db: Any, s3: Any, work_dir: str, job_id: str
) -> str:
    job = db.fetchrow("SELECT artifact_id FROM jobs WHERE id = %s", job_id)
    if job is None:
        raise ValueError(f"job {job_id} not found")
    artifact_id = job["artifact_id"]
    art = db.fetchrow(
        "SELECT s3_bucket, s3_key FROM artifacts WHERE id = %s", artifact_id
    )
    if art is None:
        raise ValueError(f"artifact {artifact_id} not found")
    Path(work_dir).mkdir(parents=True, exist_ok=True)
    suffix = Path(art["s3_key"]).suffix or ".bin"
    dest = str(Path(work_dir) / f"{job_id}{suffix}")
    s3.download_file(art["s3_bucket"], art["s3_key"], dest)
    return dest


def _extract_from_path(path: str) -> dict[str, Any]:
    m = MutagenFile(path)
    if m is None:
        raise ValueError(f"mutagen could not parse {path}")
    info = m.info
    return {
        "format": m.mime[0] if m.mime else None,
        "duration_seconds": float(info.length) if info else None,
        "bitrate": int(info.bitrate) if info and info.bitrate else None,
        "sample_rate": int(info.sample_rate) if info and info.sample_rate else None,
        "channels": int(info.channels) if info and info.channels else None,
        "tags": {k: [str(v)] for k, v in (m.tags or {})},
    }


@register_processor("extract-metadata")
async def extract_metadata(ctx: dict[str, Any], job_id: str) -> dict[str, Any]:
    """Download the artifact, run mutagen, return metadata as a dict.

    The returned dict is written verbatim into jobs.result jsonb. The
    keys match the OpenAPI JobResult shape (see
    apps/api/internal/handlers/openapi.json).
    """
    db = ctx["db"]
    s3 = ctx["s3"]
    work_dir = ctx["work_dir"]
    local_path = await _download_artifact(db, s3, work_dir, job_id)
    try:
        return _extract_from_path(local_path)
    finally:
        try:
            os.unlink(local_path)
        except FileNotFoundError:
            pass
