from __future__ import annotations

import os
import uuid
from pathlib import Path
from typing import Any

import structlog

from ..ffmpeg import FFmpegError, convert_to_wav_16k_mono
from . import register_processor

# Deterministic namespace so a redelivered convert job maps to the same
# output artifact id (idempotency — insert_artifact is ON CONFLICT DO NOTHING).
_CONVERT_NS = uuid.UUID("b2d4f6a8-1c3e-4a5b-9d7f-2e4c6a8b0d1f")

logger = structlog.get_logger(__name__)


@register_processor("convert-to-wav")
async def convert_to_wav(ctx: dict[str, Any], job_id: str) -> dict[str, Any]:
    """Transcode the source artifact to 16 kHz mono 16-bit PCM WAV.

    This is the standalone form of the conversion that transcribe/diarize
    do inline — useful as a first-class step (e.g. normalising varied
    inputs before a downstream pipeline). Writes a new artifact under
    ``converted/{org_id}/{src_artifact_id}/audio.wav`` and returns its id.
    """
    db = ctx["db"]
    s3 = ctx["s3"]
    work_dir = ctx["work_dir"]

    job = db.fetchrow("SELECT artifact_id, org_id FROM jobs WHERE id = %s", job_id)
    if job is None:
        raise ValueError(f"job {job_id} not found")
    src_artifact_id = job["artifact_id"]
    org_id = job["org_id"]

    src = db.fetchrow(
        "SELECT s3_bucket, s3_key FROM artifacts WHERE id = %s",
        src_artifact_id,
    )
    if src is None:
        raise ValueError(f"artifact {src_artifact_id} not found")

    suffix = Path(src["s3_key"]).suffix or ".bin"
    src_path = Path(work_dir) / f"{job_id}.src{suffix}"
    dst_path = Path(work_dir) / f"{job_id}.16k.wav"
    try:
        s3.download_file(src["s3_bucket"], src["s3_key"], str(src_path))
        try:
            convert_to_wav_16k_mono(src_path, dst_path)
        except FFmpegError as e:
            logger.error("worker.ffmpeg_failed", job_id=job_id, err=str(e))
            raise

        size_bytes = os.path.getsize(dst_path)
        if size_bytes == 0:
            raise FFmpegError("convert produced an empty output file")

        out_id = str(uuid.uuid5(_CONVERT_NS, job_id))
        out_key = f"converted/{org_id}/{src_artifact_id}/audio.wav"
        s3.upload_file(src["s3_bucket"], out_key, str(dst_path), content_type="audio/wav")
        db.insert_artifact(
            out_id,
            org_id,
            src["s3_bucket"],
            out_key,
            "audio/wav",
            size_bytes,
        )
        return {
            "artifact_id": out_id,
            "source_artifact_id": str(src_artifact_id),
            "content_type": "audio/wav",
            "sample_rate": 16000,
            "channels": 1,
            "size_bytes": size_bytes,
        }
    finally:
        for p in (src_path, dst_path):
            try:
                os.unlink(p)
            except FileNotFoundError:
                pass
