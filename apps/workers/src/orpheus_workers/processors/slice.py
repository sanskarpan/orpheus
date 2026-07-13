from __future__ import annotations

import json
import os
import uuid
from pathlib import Path
from typing import Any

import structlog

from ..ffmpeg import FFmpegError, slice as run_ffmpeg_slice
from . import register_processor

logger = structlog.get_logger(__name__)


@register_processor("slice")
async def slice_artifact(ctx: dict[str, Any], job_id: str) -> dict[str, Any]:
    """Extract a time range from the source artifact; create a new artifact.

    Reads ``start_seconds`` and ``end_seconds`` from ``jobs.params``
    and writes the slice to a new s3_key under
    ``slices/{org_id}/{src_artifact_id}/``. The new artifact's id
    and the slice's start/end are in the job's result.
    """
    db = ctx["db"]
    s3 = ctx["s3"]
    work_dir = ctx["work_dir"]

    job = db.fetchrow("SELECT artifact_id, org_id, params FROM jobs WHERE id = %s", job_id)
    if job is None:
        raise ValueError(f"job {job_id} not found")
    src_artifact_id = job["artifact_id"]
    org_id = job["org_id"]
    params = job["params"] or {}
    if isinstance(params, str):
        params = json.loads(params)
    start_seconds = float(params["start_seconds"])
    end_seconds = float(params["end_seconds"])

    src = db.fetchrow(
        "SELECT s3_bucket, s3_key, content_type FROM artifacts WHERE id = %s",
        src_artifact_id,
    )
    if src is None:
        raise ValueError(f"artifact {src_artifact_id} not found")

    suffix = Path(src["s3_key"]).suffix or ".bin"
    src_path = Path(work_dir) / f"{job_id}.src{suffix}"
    dst_path = Path(work_dir) / f"{job_id}.dst{suffix}"
    try:
        s3.download_file(src["s3_bucket"], src["s3_key"], str(src_path))
        try:
            run_ffmpeg_slice(src_path, dst_path, start_seconds, end_seconds)
        except FFmpegError as e:
            logger.error("worker.ffmpeg_failed", job_id=job_id, err=str(e))
            raise

        size_bytes = os.path.getsize(dst_path)
        slice_id = str(uuid.uuid4())
        slice_key = f"slices/{org_id}/{src_artifact_id}/{start_seconds}-{end_seconds}{suffix}"
        s3.upload_file(src["s3_bucket"], slice_key, str(dst_path), content_type=src["content_type"])
        db.insert_artifact(
            slice_id,
            org_id,
            src["s3_bucket"],
            slice_key,
            src["content_type"],
            size_bytes,
        )
        return {
            "slice_artifact_id": slice_id,
            "source_artifact_id": src_artifact_id,
            "start_seconds": start_seconds,
            "end_seconds": end_seconds,
            "size_bytes": size_bytes,
        }
    finally:
        for p in (src_path, dst_path):
            try:
                os.unlink(p)
            except FileNotFoundError:
                pass
