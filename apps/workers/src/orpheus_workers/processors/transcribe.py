from __future__ import annotations

import os
from pathlib import Path
from typing import Any

import structlog

from ..ffmpeg import FFmpegError, convert_to_wav_16k_mono
from ..transcribe import TranscribeError, transcribe
from . import register_processor

logger = structlog.get_logger(__name__)


@register_processor("transcribe")
async def transcribe_artifact(ctx: dict[str, Any], job_id: str) -> dict[str, Any]:
    """Download the artifact, convert to 16kHz mono wav, transcribe.

    Writes the transcript (``text``, ``segments``, ``language``,
    ``duration_seconds``) to ``jobs.result``. On any error the job
    is marked failed by the worker.
    """
    db = ctx["db"]
    s3 = ctx["s3"]
    work_dir = ctx["work_dir"]

    job = db.fetchrow("SELECT artifact_id, org_id FROM jobs WHERE id = %s", job_id)
    if job is None:
        raise ValueError(f"job {job_id} not found")
    artifact_id = job["artifact_id"]

    art = db.fetchrow("SELECT s3_bucket, s3_key FROM artifacts WHERE id = %s", artifact_id)
    if art is None:
        raise ValueError(f"artifact {artifact_id} not found")

    Path(work_dir).mkdir(parents=True, exist_ok=True)
    src_suffix = Path(art["s3_key"]).suffix or ".bin"
    src_path = Path(work_dir) / f"{job_id}.src{src_suffix}"
    wav_path = Path(work_dir) / f"{job_id}.wav"
    try:
        s3.download_file(art["s3_bucket"], art["s3_key"], str(src_path))
        try:
            convert_to_wav_16k_mono(src_path, wav_path)
        except FFmpegError as e:
            logger.error("worker.ffmpeg_convert_failed", job_id=job_id, err=str(e))
            raise

        model_size = os.environ.get("ORPHEUS_WORKER_WHISPER_MODEL", "tiny.en")
        model_dir = os.environ.get("ORPHEUS_WORKER_WHISPER_DIR") or None
        try:
            return transcribe(wav_path, model_size=model_size, model_dir=model_dir)
        except TranscribeError as e:
            logger.error("worker.whisper_failed", job_id=job_id, err=str(e))
            raise
    finally:
        for p in (src_path, wav_path):
            try:
                os.unlink(p)
            except FileNotFoundError:
                pass
