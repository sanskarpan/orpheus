"""Speaker enrollment / voiceprints processor (PRD 03 §4.8).

``speaker.enroll`` embeds a labeled audio sample (ECAPA via the Modal diarize
``/embed`` endpoint, or the stub) and stores a unit-norm voiceprint in the
tenant-scoped ``speaker_profiles`` table. Voiceprints are biometric data:
enrollment is **explicit and consent-gated** — the processor refuses without
``params.consent=true``. Recognition lives in ``audio.diarize`` (``identify``).
"""

from __future__ import annotations

from pathlib import Path
from typing import Any

import structlog

from ..ffmpeg import convert_to_wav_16k_mono
from ..voiceprints import get_embedder
from . import register_processor
from .text_ops import _params

logger = structlog.get_logger(__name__)


@register_processor(
    "speaker.enroll",
    display_name="Enroll Speaker",
    description="Enroll a speaker voiceprint from a labeled audio sample (consent-gated).",
    tier="gpu_a10g",
    timeout_seconds=600,
    cost_per_job_usd=0.01,
    cacheable=False,
    model_id="ecapa",
    model_version_id="ecapa-agglomerative-1",
)
async def enroll_proc(ctx: dict[str, Any], job_id: str) -> dict[str, Any]:
    db = ctx["db"]
    s3 = ctx["s3"]
    work_dir = Path(ctx["work_dir"])
    job = db.fetchrow("SELECT id, org_id, artifact_id, params FROM jobs WHERE id = %s", job_id)
    if job is None:
        raise ValueError(f"job {job_id} not found")
    params = _params(job)

    name = str(params.get("name", "")).strip()
    if not name:
        raise ValueError("speaker.enroll requires params.name")
    if not bool(params.get("consent")):
        # Biometric enrollment must be explicit — never implicit from diarization.
        raise ValueError("speaker.enroll requires explicit params.consent=true (biometric data)")

    artifact_id = job["artifact_id"] or params.get("artifact_id")
    if not artifact_id:
        raise ValueError("speaker.enroll requires the source audio artifact")
    art = db.fetchrow("SELECT s3_bucket, s3_key FROM artifacts WHERE id = %s AND org_id = %s", artifact_id, job["org_id"])
    if art is None:
        raise ValueError(f"artifact {artifact_id} not found")

    work_dir.mkdir(parents=True, exist_ok=True)
    suffix = Path(art["s3_key"]).suffix or ".bin"
    src = work_dir / f"enroll-{job_id}.src{suffix}"
    wav = work_dir / f"enroll-{job_id}.wav"
    try:
        s3.download_file(art["s3_bucket"], art["s3_key"], str(src))
        convert_to_wav_16k_mono(src, wav)
        embedder = get_embedder()
        embedding = embedder.embed(wav)
    finally:
        for p in (src, wav):
            p.unlink(missing_ok=True)
    if not embedding:
        raise ValueError("failed to compute speaker embedding")

    row = db.fetchrow(
        """
        INSERT INTO speaker_profiles (org_id, name, embedding, dim, consent, model_version_id)
        VALUES (%s, %s, %s, %s, true, %s)
        ON CONFLICT (org_id, name) DO UPDATE SET
            embedding = EXCLUDED.embedding,
            dim = EXCLUDED.dim,
            sample_count = speaker_profiles.sample_count + 1,
            model_version_id = EXCLUDED.model_version_id,
            updated_at = now()
        RETURNING id::text AS id
        """,
        job["org_id"], name, embedding, len(embedding), embedder.model_version_id,
    )
    return {
        "speaker_id": row["id"],
        "name": name,
        "dim": len(embedding),
        "model_version_id": embedder.model_version_id,
    }
