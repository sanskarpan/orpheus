from __future__ import annotations

import os
import wave
from pathlib import Path
from typing import Any

import structlog

from ..ffmpeg import FFmpegError, convert_to_wav_16k_mono
from ..ffmpeg import slice as ffmpeg_slice
from ..transcribe import TranscribeError, transcribe
from . import register_processor

logger = structlog.get_logger(__name__)

DEFAULT_CHUNK_SECONDS = 60


@register_processor("transcribe")
async def transcribe_artifact(ctx: dict[str, Any], job_id: str) -> dict[str, Any]:
    """Download the artifact, transcribe with whisper, return the result.

    For long files (duration > ``chunk_seconds``), splits the wav into
    ``chunk_seconds``-sized pieces, transcribes each, and concatenates
    the transcripts. ``chunk_seconds`` is read from
    ``jobs.params.chunk_seconds`` (default 60). The on-wire shape is
    unchanged: ``{text, segments, language, duration_seconds}``.
    """
    db = ctx["db"]
    s3 = ctx["s3"]
    work_dir = ctx["work_dir"]

    job = db.fetchrow("SELECT artifact_id, params FROM jobs WHERE id = %s", job_id)
    if job is None:
        raise ValueError(f"job {job_id} not found")
    artifact_id = job["artifact_id"]
    params = job["params"] or {}
    if isinstance(params, str):
        import json as _json

        params = _json.loads(params)
    chunk_seconds = float(params.get("chunk_seconds", DEFAULT_CHUNK_SECONDS))

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

        duration = _wav_duration(wav_path)
        if duration <= chunk_seconds:
            return _transcribe_one(wav_path, offset=0.0)

        model_size = os.environ.get("ORPHEUS_WORKER_WHISPER_MODEL", "tiny.en")
        model_dir = os.environ.get("ORPHEUS_WORKER_WHISPER_DIR") or None
        n_chunks = int(duration // chunk_seconds) + (1 if duration % chunk_seconds else 0)
        all_segments: list[dict] = []
        all_text: list[str] = []
        language = "en"
        for i in range(n_chunks):
            start = i * chunk_seconds
            end = min(start + chunk_seconds, duration)
            chunk_path = Path(work_dir) / f"{job_id}.chunk{i}.wav"
            try:
                ffmpeg_slice(wav_path, chunk_path, start, end)
                result = transcribe(chunk_path, model_size=model_size, model_dir=model_dir)
            except (FFmpegError, TranscribeError) as e:
                logger.error("worker.chunk_failed", job_id=job_id, chunk=i, err=str(e))
                raise
            finally:
                try:
                    os.unlink(chunk_path)
                except FileNotFoundError:
                    pass
            for seg in result.get("segments") or []:
                all_segments.append(
                    {
                        "start": float(seg.get("start", 0.0)) + start,
                        "end": float(seg.get("end", 0.0)) + start,
                        "text": seg.get("text", ""),
                    }
                )
            text = result.get("text", "").strip()
            if text:
                all_text.append(text)
            if i == 0 and result.get("language"):
                language = result["language"]
        return {
            "text": " ".join(all_text).strip(),
            "segments": all_segments,
            "language": language,
            "duration_seconds": duration,
        }
    finally:
        for p in (src_path, wav_path):
            try:
                os.unlink(p)
            except FileNotFoundError:
                pass


def _transcribe_one(wav_path: Path, offset: float) -> dict[str, Any]:
    """Transcribe a single wav, optionally shifting segment timestamps by ``offset``."""
    model_size = os.environ.get("ORPHEUS_WORKER_WHISPER_MODEL", "tiny.en")
    model_dir = os.environ.get("ORPHEUS_WORKER_WHISPER_DIR") or None
    result = transcribe(wav_path, model_size=model_size, model_dir=model_dir)
    for seg in result.get("segments") or []:
        seg["start"] = float(seg.get("start", 0.0)) + offset
        seg["end"] = float(seg.get("end", 0.0)) + offset
    return result


def _wav_duration(path: Path) -> float:
    """Return the duration in seconds of a 16kHz mono wav file."""
    with wave.open(str(path), "rb") as w:
        frames = w.getnframes()
        rate = w.getframerate()
        if rate == 0:
            return 0.0
        return frames / float(rate)
