from __future__ import annotations

import json
import os
import uuid
from pathlib import Path
from typing import Any

import structlog

from ..ffmpeg import FFmpegError, slice as run_ffmpeg_slice
from ..validation import parse_time_range
from . import register_processor

# Deterministic namespace so a redelivered slice job maps to the same
# artifact id (idempotency — see below).
_SLICE_NS = uuid.UUID("6f1e5b8a-2c3d-4e5f-8a9b-0c1d2e3f4a5b")

logger = structlog.get_logger(__name__)


@register_processor(
    "slice",
    display_name="Slice",
    description="Extract a time range from a source artifact into a new artifact.",
    tier="cpu_tiny",
    timeout_seconds=120,
    cost_per_job_usd=0.0005,
    model_id="ffmpeg",
    model_version_id="ffmpeg-1",
)
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
    # Validate untrusted params: reject NaN/Inf/negative/inverted ranges
    # and spans beyond the safety cap before they reach ffmpeg args.
    start_seconds, end_seconds = parse_time_range(params)

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
        # ffmpeg -c copy can exit 0 while producing an empty/truncated
        # file for some container/codec + range combinations; refuse to
        # register a 0-byte artifact as a successful slice.
        if size_bytes == 0:
            raise FFmpegError("slice produced an empty output file")
        # Deterministic id/key so a redelivered job (worker nak → JetStream
        # redelivery) re-uses the same artifact instead of inserting a
        # duplicate; insert_artifact is ON CONFLICT DO NOTHING.
        slice_id = str(uuid.uuid5(_SLICE_NS, job_id))
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
            "slice_artifact_id": str(slice_id),
            "source_artifact_id": str(src_artifact_id),
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
