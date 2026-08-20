"""Highlight-reel / clip extraction processor (PRD 16 #361).

``audio.highlights`` asks the LLM to pick the most highlight-worthy moments of a
transcript (by segment range), maps them to timestamps, and — when
``params.extract_clips`` is set and the source audio is available — cuts each
moment into its own audio-clip artifact. Reuses the ``text_ops`` LLM-analysis
pattern, the ffmpeg ``slice`` helper, and the artifact-write helper.
"""

from __future__ import annotations

from pathlib import Path
from typing import Any

import structlog

from ..ffmpeg import convert_to_wav_16k_mono
from ..ffmpeg import slice as ffmpeg_slice
from ..llm import get_llm
from ..llm import manifest_identity as _llm_manifest
from . import register_processor
from .audio_ops import _insert_artifact
from .text_ops import _ANALYSIS_SYSTEM, _analyze_json, _load_transcript, _MAX_INPUT_CHARS, _params

logger = structlog.get_logger(__name__)

_LLM_MODEL_ID, _LLM_MODEL_VERSION = _llm_manifest()


def _plan_highlights(
    data: Any, segments: list[dict[str, Any]], max_n: int = 5
) -> list[dict[str, Any]]:
    """Map LLM-proposed highlight ranges (segment indices) → timestamped moments.

    Clamps indices, drops invalid/empty ranges, orders by start, and caps at
    ``max_n``. Returns ``[{start, end, title, reason}]`` (empty if none valid).
    """
    n = len(segments)
    if n == 0:
        return []
    proposed = data.get("highlights") if isinstance(data, dict) else None
    out: list[dict[str, Any]] = []
    seen: set[tuple[int, int]] = set()
    for h in proposed or []:
        if not isinstance(h, dict):
            continue
        try:
            a = int(h.get("start_index", h.get("index", 0)))
            b = int(h.get("end_index", a))
        except (TypeError, ValueError):
            continue
        a = max(0, min(a, n - 1))
        b = max(a, min(b, n - 1))
        if (a, b) in seen:
            continue
        seen.add((a, b))
        out.append(
            {
                "start": round(float(segments[a].get("start", 0.0)), 3),
                "end": round(float(segments[b].get("end", segments[b].get("start", 0.0))), 3),
                "title": str(h.get("title", "")).strip() or "Highlight",
                "reason": str(h.get("reason", "")).strip(),
            }
        )
    out.sort(key=lambda x: x["start"])
    return out[:max_n]


@register_processor(
    "audio.highlights",
    display_name="Highlight Reel",
    description="LLM-picked highlight moments of a transcript, with optional audio clips.",
    tier="cpu_small",
    timeout_seconds=600,
    cost_per_job_usd=0.005,
    cacheable=False,
    model_id=_LLM_MODEL_ID,
    model_version_id=_LLM_MODEL_VERSION,
)
async def highlights_proc(ctx: dict[str, Any], job_id: str) -> dict[str, Any]:
    db = ctx["db"]
    job = db.fetchrow("SELECT id, org_id, artifact_id, params FROM jobs WHERE id = %s", job_id)
    if job is None:
        raise ValueError(f"job {job_id} not found")
    params = _params(job)
    transcript = _load_transcript(ctx, job, params)
    segments = transcript.get("segments") or []
    max_n = int(params.get("max_highlights", 5))
    llm = get_llm()

    if not segments:
        return {"highlights": [], "model_version_id": llm.model_version_id}

    lines = "\n".join(
        f"{i}: [{float(s.get('start', 0.0)):.0f}s] {str(s.get('text', '')).strip()}"
        for i, s in enumerate(segments)
    )[:_MAX_INPUT_CHARS]
    data = _analyze_json(
        llm,
        _ANALYSIS_SYSTEM,
        f'Return {{"highlights":[{{"start_index":<seg>,"end_index":<seg>,"title":"...",'
        f'"reason":"<why this is a highlight>"}}]}} — the {max_n} most notable, '
        "quotable, or decision moments.\n<transcript>\n" + lines + "\n</transcript>",
        max_tokens=700,
    )
    highlights = _plan_highlights(data, segments, max_n)

    # Optional: cut each highlight into its own audio-clip artifact.
    if params.get("extract_clips") and highlights:
        _extract_clips(ctx, job, params, highlights)

    return {"highlights": highlights, "model_version_id": llm.model_version_id}


def _extract_clips(
    ctx: dict[str, Any], job: dict, params: dict, highlights: list[dict[str, Any]]
) -> None:
    """Cut each highlight range from the source audio → clip artifact (in place)."""
    db = ctx["db"]
    s3 = ctx["s3"]
    bucket = ctx["bucket"]
    work_dir = Path(ctx["work_dir"])
    artifact_id = job["artifact_id"] or params.get("artifact_id")
    if not artifact_id:
        return
    art = db.fetchrow("SELECT s3_bucket, s3_key FROM artifacts WHERE id = %s AND org_id = %s", artifact_id, job["org_id"])
    if art is None:
        return
    work_dir.mkdir(parents=True, exist_ok=True)
    suffix = Path(art["s3_key"]).suffix or ".bin"
    src = work_dir / f"hl-{job['id']}.src{suffix}"
    wav = work_dir / f"hl-{job['id']}.wav"
    try:
        s3.download_file(art["s3_bucket"], art["s3_key"], str(src))
        convert_to_wav_16k_mono(src, wav)
        for i, h in enumerate(highlights):
            clip = work_dir / f"hl-{job['id']}.{i}.wav"
            try:
                ffmpeg_slice(wav, clip, h["start"], h["end"])
                data = clip.read_bytes()
                key = f"highlights/{job['org_id']}/{job['id']}/{i}.wav"
                s3.upload_file(bucket, key, str(clip), "audio/wav")
                h["clip_artifact_id"] = _insert_artifact(
                    db, job["org_id"], bucket, key, data, "audio/wav"
                )
            except Exception as e:  # one clip failing must not sink the rest
                logger.warning("worker.highlight_clip_failed", job_id=job["id"], i=i, err=str(e))
            finally:
                clip.unlink(missing_ok=True)
    finally:
        for p in (src, wav):
            p.unlink(missing_ok=True)
