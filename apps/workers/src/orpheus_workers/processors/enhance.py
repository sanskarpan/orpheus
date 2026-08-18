"""Audio enhancement processor (PRD 04).

``audio.enhance`` cleans a source audio artifact and emits a new **enhanced audio
artifact** plus quality metrics, composable ahead of ``transcribe`` /
``audio.diarize`` via ``source_job_id``/``artifact_id``. Neural modes run on the
Modal GPU service; the default ``denoise`` runs the dependency-free in-worker
spectral-subtraction path (``enhance.get_enhancer``).
"""

from __future__ import annotations

from pathlib import Path
from typing import Any

import structlog

from ..enhance import get_enhancer
from ..enhance import manifest_identity as _enhance_manifest
from ..ffmpeg import convert_to_wav_16k_mono
from . import register_processor
from .audio_ops import _insert_artifact
from .text_ops import _params

logger = structlog.get_logger(__name__)

_ENHANCE_MODEL_ID, _ENHANCE_MODEL_VERSION = _enhance_manifest()
_VALID_MODES = {"denoise", "dereverb", "isolate", "background_voice", "telephony", "accent"}


@register_processor(
    "audio.enhance",
    display_name="Audio Enhancement",
    description="Clean audio (denoise/dereverb/isolate/…) → enhanced audio artifact + metrics.",
    tier="cpu_medium",
    timeout_seconds=900,
    cost_per_job_usd=0.01,
    cacheable=True,
    model_id=_ENHANCE_MODEL_ID,
    model_version_id=_ENHANCE_MODEL_VERSION,
)
async def enhance_proc(ctx: dict[str, Any], job_id: str) -> dict[str, Any]:
    db = ctx["db"]
    s3 = ctx["s3"]
    bucket = ctx["bucket"]
    work_dir = Path(ctx["work_dir"])
    job = db.fetchrow("SELECT id, org_id, artifact_id, params FROM jobs WHERE id = %s", job_id)
    if job is None:
        raise ValueError(f"job {job_id} not found")
    params = _params(job)
    mode = str(params.get("mode", "denoise")).strip().lower()
    if mode not in _VALID_MODES:
        raise ValueError(f"unknown enhance mode {mode!r}; valid: {sorted(_VALID_MODES)}")

    artifact_id = job["artifact_id"] or params.get("artifact_id")
    if not artifact_id:
        raise ValueError("audio.enhance requires the source audio artifact")
    art = db.fetchrow("SELECT s3_bucket, s3_key FROM artifacts WHERE id = %s AND org_id = %s", artifact_id, job["org_id"])
    if art is None:
        raise ValueError(f"artifact {artifact_id} not found")

    work_dir.mkdir(parents=True, exist_ok=True)
    suffix = Path(art["s3_key"]).suffix or ".bin"
    src = work_dir / f"enh-{job_id}.src{suffix}"
    wav = work_dir / f"enh-{job_id}.wav"
    out = work_dir / f"enh-{job_id}.clean.wav"
    try:
        s3.download_file(art["s3_bucket"], art["s3_key"], str(src))
        convert_to_wav_16k_mono(src, wav)
        enhancer = get_enhancer(mode)
        result = enhancer.enhance(wav, out, mode)
        data = out.read_bytes()
        key = f"enhanced-audio/{job['org_id']}/{job_id}.wav"
        s3.upload_file(bucket, key, str(out), "audio/wav")
        enhanced_id = _insert_artifact(db, job["org_id"], bucket, key, data, "audio/wav")
    finally:
        for p in (src, wav, out):
            p.unlink(missing_ok=True)

    resp: dict[str, Any] = {
        "enhanced_audio_artifact_id": enhanced_id,
        "mode": mode,
        "metrics": result.get("metrics", {}),
        "model_version_id": enhancer.model_version_id,
    }
    if result.get("warnings"):
        resp["warnings"] = result["warnings"]
    return resp
