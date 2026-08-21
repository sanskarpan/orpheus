"""Dubbing / overdub processor (PRD 16 #367).

``audio.dub`` turns a transcript into a synthesized speech track — optionally
translated into a target language first — using the neural TTS (Kokoro on Modal;
a silent stub without it). Emits a dubbed-audio artifact. This uses fixed catalog
voices only; speaker voice-cloning is a separate, consent-gated capability and is
deliberately NOT done here.
"""

from __future__ import annotations

from pathlib import Path
from typing import Any

import structlog

from ..llm import get_llm
from ..tts import get_tts
from . import register_processor
from .audio_ops import _insert_artifact
from .text_ops import _load_transcript, _MAX_INPUT_CHARS, _params

logger = structlog.get_logger(__name__)


@register_processor(
    "audio.dub",
    display_name="Dubbing / Overdub",
    description="Synthesize a (optionally translated) speech track from a transcript.",
    tier="cpu_medium",
    timeout_seconds=900,
    cost_per_job_usd=0.02,
    cacheable=False,
    model_id="orpheus-tts",
    model_version_id="kokoro-82m-1",
)
async def dub_proc(ctx: dict[str, Any], job_id: str) -> dict[str, Any]:
    db = ctx["db"]
    s3 = ctx["s3"]
    bucket = ctx["bucket"]
    work_dir = Path(ctx["work_dir"])
    job = db.fetchrow("SELECT id, org_id, artifact_id, params FROM jobs WHERE id = %s", job_id)
    if job is None:
        raise ValueError(f"job {job_id} not found")
    params = _params(job)
    transcript = _load_transcript(ctx, job, params)
    text = (transcript.get("text", "") or "")[:_MAX_INPUT_CHARS]
    if not text.strip():
        raise ValueError("transcript has no text to dub")

    target_language = params.get("target_language")
    voice = params.get("voice")
    warnings: list[str] = []

    if target_language:
        try:
            text = get_llm().translate(text, target_language, params.get("source_language", "auto"))
        except Exception as e:
            logger.warning("worker.dub_translate_failed", job_id=job_id, err=str(e))
            warnings.append("translation_unavailable_dubbed_source_language")

    tts = get_tts()
    audio, sr = tts.synth(text, voice)
    if not audio:
        raise ValueError("TTS produced no audio")

    work_dir.mkdir(parents=True, exist_ok=True)
    out = work_dir / f"dub-{job_id}.wav"
    try:
        out.write_bytes(audio)
        key = f"dubbed-audio/{job['org_id']}/{job_id}.wav"
        s3.upload_file(bucket, key, str(out), "audio/wav")
        dubbed_id = _insert_artifact(db, job["org_id"], bucket, key, audio, "audio/wav")
    finally:
        out.unlink(missing_ok=True)

    result: dict[str, Any] = {
        "dubbed_audio_artifact_id": dubbed_id,
        "target_language": target_language,
        "voice": voice,
        "sample_rate": sr,
        "model_version_id": tts.model_version_id,
    }
    if warnings:
        result["warnings"] = warnings
    return result
