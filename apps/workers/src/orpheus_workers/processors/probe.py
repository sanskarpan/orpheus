from __future__ import annotations

import os
from pathlib import Path
from typing import Any

import structlog

from ..ffprobe import FFprobeError, extract_audio_metadata, probe as run_ffprobe
from . import register_processor

logger = structlog.get_logger(__name__)


@register_processor("probe")
async def probe_artifact(ctx: dict[str, Any], job_id: str) -> dict[str, Any]:
    """Download the artifact, run ffprobe, write metadata back.

    Updates the artifacts row's probe_status to 'completed' and
    the jobs row to 'completed' with the parsed metadata as the
    result. On ffprobe error, marks both rows 'failed'.
    """
    db = ctx["db"]
    s3 = ctx["s3"]
    work_dir = ctx["work_dir"]

    job = db.fetchrow("SELECT artifact_id FROM jobs WHERE id = %s", job_id)
    if job is None:
        raise ValueError(f"job {job_id} not found")
    artifact_id = job["artifact_id"]

    art = db.fetchrow("SELECT s3_bucket, s3_key FROM artifacts WHERE id = %s", artifact_id)
    if art is None:
        raise ValueError(f"artifact {artifact_id} not found")

    Path(work_dir).mkdir(parents=True, exist_ok=True)
    suffix = Path(art["s3_key"]).suffix or ".bin"
    local_path = Path(work_dir) / f"{job_id}{suffix}"
    try:
        s3.download_file(art["s3_bucket"], art["s3_key"], str(local_path))
        try:
            data = run_ffprobe(local_path)
            meta = extract_audio_metadata(data)
            if meta.get("codec") is None:
                raise FFprobeError("no audio stream found")
        except FFprobeError as e:
            logger.error("worker.ffprobe_failed", job_id=job_id, err=str(e))
            db.mark_artifact_probe_failed(artifact_id)
            raise
        db.mark_artifact_probed(
            artifact_id,
            meta["codec"],
            meta["sample_rate"],
            meta["channels"],
            meta["duration_seconds"],
        )
        return meta
    finally:
        try:
            os.unlink(local_path)
        except FileNotFoundError:
            pass
