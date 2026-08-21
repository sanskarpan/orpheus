"""Audio-edit-by-text processors (PRD 16 §Descript-class, #365).

``audio.edit`` with ``mode="remove_fillers"`` removes filler words ("um", "uh",
…) from the media using the transcript's word timestamps: the filler time ranges
are cut with ffmpeg (audio genuinely shortened) and the transcript is rebuilt with
those words dropped and every remaining timestamp re-indexed to the new timeline.
Emits an edited-audio artifact + the cleaned transcript. Reuses the artifact-write
pattern and the word-timestamp machinery already used by ``audio.redact``.
"""

from __future__ import annotations

import re
from pathlib import Path
from typing import Any

from ..ffmpeg import convert_to_wav_16k_mono, cut_ranges
from . import register_processor
from .audio_ops import _insert_artifact
from .text_ops import _load_transcript, _params

# Conservative default filler set — clear disfluencies only, to avoid cutting
# meaningful words. Extendable per-job via params.fillers.
_DEFAULT_FILLERS = {"um", "uh", "umm", "uhh", "erm", "er", "ah", "eh", "hmm", "mm", "mhm"}


def _norm(word: str) -> str:
    return "".join(c for c in word.lower() if c.isalnum())


def _filler_set(params: dict[str, Any]) -> set[str]:
    extra = params.get("fillers")
    base = set(_DEFAULT_FILLERS)
    if isinstance(extra, list):
        base |= {_norm(str(w)) for w in extra if _norm(str(w))}
    return base


def _merge(ranges: list[tuple[float, float]]) -> list[tuple[float, float]]:
    valid = sorted((s, e) for s, e in ranges if e > s)
    out: list[tuple[float, float]] = []
    for s, e in valid:
        if out and s <= out[-1][1] + 1e-6:
            out[-1] = (out[-1][0], max(out[-1][1], e))
        else:
            out.append((s, e))
    return out


def plan_filler_edit(
    transcript: dict[str, Any], fillers: set[str], pad: float = 0.02
) -> tuple[list[tuple[float, float]], list[dict[str, Any]], int]:
    """Compute (cut_ranges, cleaned_segments, removed_count).

    ``cleaned_segments`` have filler words dropped and all timestamps re-indexed to
    the post-cut timeline (each time shifted left by the removed duration before it).
    """
    ranges: list[tuple[float, float]] = []
    removed = 0
    for seg in transcript.get("segments") or []:
        for w in seg.get("words") or []:
            if _norm(str(w.get("word", ""))) in fillers:
                ranges.append(
                    (max(0.0, float(w.get("start", 0.0)) - pad), float(w.get("end", 0.0)) + pad)
                )
                removed += 1
    ranges = _merge(ranges)

    def shift(t: float) -> float:
        removed_before = sum(max(0.0, min(e, t) - s) for s, e in ranges if s < t)
        return round(t - removed_before, 3)

    cleaned: list[dict[str, Any]] = []
    for seg in transcript.get("segments") or []:
        new_words = []
        for w in seg.get("words") or []:
            if _norm(str(w.get("word", ""))) in fillers:
                continue
            new_words.append(
                {
                    **w,
                    "start": shift(float(w.get("start", 0.0))),
                    "end": shift(float(w.get("end", 0.0))),
                }
            )
        if new_words:
            cleaned.append(
                {
                    "start": new_words[0]["start"],
                    "end": new_words[-1]["end"],
                    "text": re.sub(
                        r"\s+", " ", " ".join(str(w.get("word", "")) for w in new_words)
                    ).strip(),
                    "words": new_words,
                }
            )
    return ranges, cleaned, removed


@register_processor(
    "audio.edit",
    display_name="Audio Edit (text-driven)",
    description="Remove filler words from media by transcript, emitting a shorter clean cut.",
    tier="cpu_medium",
    timeout_seconds=900,
    cost_per_job_usd=0.01,
    cacheable=False,
    model_id="orpheus-audio-edit",
    model_version_id="orpheus-audio-edit-1",
)
async def edit_proc(ctx: dict[str, Any], job_id: str) -> dict[str, Any]:
    db = ctx["db"]
    s3 = ctx["s3"]
    bucket = ctx["bucket"]
    work_dir = Path(ctx["work_dir"])
    job = db.fetchrow("SELECT id, org_id, artifact_id, params FROM jobs WHERE id = %s", job_id)
    if job is None:
        raise ValueError(f"job {job_id} not found")
    params = _params(job)
    mode = str(params.get("mode", "remove_fillers")).strip().lower()
    if mode != "remove_fillers":
        raise ValueError(f"unknown audio.edit mode {mode!r} (supported: remove_fillers)")

    transcript = _load_transcript(ctx, job, params)
    ranges, cleaned, removed = plan_filler_edit(transcript, _filler_set(params))

    artifact_id = job["artifact_id"] or params.get("artifact_id")
    if not artifact_id:
        raise ValueError("audio.edit requires the source audio artifact")
    art = db.fetchrow(
        "SELECT s3_bucket, s3_key FROM artifacts WHERE id = %s AND org_id = %s",
        artifact_id,
        job["org_id"],
    )
    if art is None:
        raise ValueError(f"artifact {artifact_id} not found")

    work_dir.mkdir(parents=True, exist_ok=True)
    suffix = Path(art["s3_key"]).suffix or ".bin"
    src = work_dir / f"edit-{job_id}.src{suffix}"
    wav = work_dir / f"edit-{job_id}.wav"
    out = work_dir / f"edit-{job_id}.clean.wav"
    try:
        s3.download_file(art["s3_bucket"], art["s3_key"], str(src))
        convert_to_wav_16k_mono(src, wav)
        cut_ranges(wav, out, ranges)
        data = out.read_bytes()
        key = f"edited-audio/{job['org_id']}/{job_id}.wav"
        s3.upload_file(bucket, key, str(out), "audio/wav")
        edited_id = _insert_artifact(db, job["org_id"], bucket, key, data, "audio/wav")
    finally:
        for p in (src, wav, out):
            p.unlink(missing_ok=True)

    return {
        "edited_audio_artifact_id": edited_id,
        "mode": mode,
        "fillers_removed": removed,
        "removed_ranges": [{"start": round(s, 3), "end": round(e, 3)} for s, e in ranges],
        "segments": cleaned,
        "text": " ".join(s["text"] for s in cleaned).strip(),
        "model_version_id": "orpheus-audio-edit-1",
    }
