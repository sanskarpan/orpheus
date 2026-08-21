"""Audio-intelligence processors (PRD 13).

Adds capabilities on top of the existing analysis surface, reusing the
`text_ops` LLM-analysis pattern and the `redact` span-masking machinery:

- ``audio.chapters`` — structured, timestamped chapters (promotes the old
  ``text.summarize`` ``mode="chapters"`` into a first-class, timestamped output).
- ``text.moderate`` — profanity / content moderation. A dependency-free lexicon
  engine (default) masks profane spans via the redaction masking modes; an ``llm``
  engine returns moderation-category scores + flagged spans.

Each degrades gracefully (a bad LLM response never hard-fails the job) and records
the ``model_version_id`` that produced it.
"""

from __future__ import annotations

import hashlib
import os
import re
from pathlib import Path
from typing import Any

import structlog

from ..ffmpeg import convert_to_wav_16k_mono, mute_ranges
from ..llm import get_llm
from ..llm import manifest_identity as _llm_manifest
from ..redact import DEFAULT_ENTITIES, Span, _mask_for, get_detector, maybe_redact
from ..senses import StubSenseAnalyzer, get_sense_analyzer
from ..senses import manifest_identity as _sense_manifest
from . import register_processor
from .text_ops import _ANALYSIS_SYSTEM, _analyze_json, _load_transcript, _MAX_INPUT_CHARS, _params

logger = structlog.get_logger(__name__)

_LLM_MODEL_ID, _LLM_MODEL_VERSION = _llm_manifest()
_SENSE_MODEL_ID, _SENSE_MODEL_VERSION = _sense_manifest()


# --- auto-chapters (§4.6) ---------------------------------------------------


def _build_chapters(data: Any, segments: list[dict[str, Any]]) -> list[dict[str, Any]]:
    """Map LLM-proposed chapter boundaries (segment indices) → timestamped chapters.

    Boundaries are clamped/sorted/deduped and the first chapter is forced to start
    at segment 0. Each chapter's ``end`` is the next chapter's start (last chapter
    runs to the final segment's end). Falls back to a single whole-file chapter
    when the model output is unusable, so a caller always gets a valid structure.
    """
    n = len(segments)

    def _seg_start(i: int) -> float:
        return float(segments[i].get("start", 0.0))

    def _seg_end(i: int) -> float:
        return float(segments[i].get("end", segments[i].get("start", 0.0)))

    proposed = data.get("chapters") if isinstance(data, dict) else None
    boundaries: list[tuple[int, str, str]] = []
    if isinstance(proposed, list):
        for ch in proposed:
            if not isinstance(ch, dict):
                continue
            try:
                idx = int(ch.get("start_index", ch.get("index", 0)))
            except (TypeError, ValueError):
                continue
            idx = max(0, min(idx, n - 1))
            title = str(ch.get("title", "")).strip() or "Chapter"
            summary = str(ch.get("summary", "")).strip()
            boundaries.append((idx, title, summary))

    # dedupe by start index (keep first), ensure a chapter starts at 0, sort
    seen: dict[int, tuple[str, str]] = {}
    for idx, title, summary in boundaries:
        seen.setdefault(idx, (title, summary))
    if 0 not in seen:
        seen[0] = ("Introduction", "")
    ordered = sorted(seen.items())

    if n == 0 or not ordered:
        return [{"start": 0.0, "end": 0.0, "title": "Full transcript", "summary": ""}]

    chapters: list[dict[str, Any]] = []
    for pos, (idx, (title, summary)) in enumerate(ordered):
        end_idx = ordered[pos + 1][0] - 1 if pos + 1 < len(ordered) else n - 1
        end_idx = max(idx, min(end_idx, n - 1))
        chapters.append(
            {
                "start": round(_seg_start(idx), 3),
                "end": round(_seg_end(end_idx), 3),
                "title": title,
                "summary": summary,
            }
        )
    return chapters


def _segments_for_prompt(segments: list[dict[str, Any]]) -> str:
    lines = []
    for i, seg in enumerate(segments):
        t = float(seg.get("start", 0.0))
        text = str(seg.get("text", "")).strip()
        lines.append(f"{i}: [{t:.0f}s] {text}")
    return "\n".join(lines)[:_MAX_INPUT_CHARS]


@register_processor(
    "audio.chapters",
    display_name="Auto Chapters",
    description="Structured, timestamped chapters segmented from a transcript.",
    tier="cpu_small",
    timeout_seconds=300,
    cost_per_job_usd=0.003,
    cacheable=False,
    model_id=_LLM_MODEL_ID,
    model_version_id=_LLM_MODEL_VERSION,
)
async def chapters_proc(ctx: dict[str, Any], job_id: str) -> dict[str, Any]:
    db = ctx["db"]
    job = db.fetchrow("SELECT org_id, artifact_id, params FROM jobs WHERE id = %s", job_id)
    if job is None:
        raise ValueError(f"job {job_id} not found")
    params = _params(job)
    transcript = _load_transcript(ctx, job, params)
    maybe_redact(transcript, params)  # PRD 08: redact before the (external) LLM
    segments = transcript.get("segments") or []
    llm = get_llm()
    if not segments:
        # No timing → a single whole-file chapter titled from a short summary.
        text = (transcript.get("text", "") or "").strip()
        title = "Full transcript" if text else "Empty"
        return {
            "chapters": [{"start": 0.0, "end": 0.0, "title": title, "summary": text[:200]}],
            "model_version_id": llm.model_version_id,
        }
    user = (
        'Return {"chapters":[{"start_index":<segment index>,"title":"...",'
        '"summary":"<one line>"}]}. Split the transcript into 3-8 titled chapters '
        "at topic shifts. start_index is the segment where each chapter begins; the "
        "first chapter must start at index 0.\n<transcript>\n"
        f"{_segments_for_prompt(segments)}\n</transcript>"
    )
    data = _analyze_json(llm, _ANALYSIS_SYSTEM, user, max_tokens=800)
    return {"chapters": _build_chapters(data, segments), "model_version_id": llm.model_version_id}


# --- profanity / content moderation (§4.4) ----------------------------------

# A compact base lexicon (extendable per-job via params.lexicon or the
# ORPHEUS_PROFANITY_LEXICON env). Deliberately conservative to avoid false mutes;
# matched case-insensitively on whole words.
_BASE_PROFANITY = {
    "fuck",
    "fucking",
    "fucker",
    "shit",
    "shitty",
    "bitch",
    "bastard",
    "asshole",
    "dick",
    "piss",
    "cunt",
    "damn",
    "crap",
    "prick",
    "slut",
    "whore",
}


def _profanity_lexicon(params: dict[str, Any]) -> set[str]:
    lex = set(_BASE_PROFANITY)
    env = os.environ.get("ORPHEUS_PROFANITY_LEXICON", "")
    lex |= {w.strip().lower() for w in env.split(",") if w.strip()}
    extra = params.get("lexicon")
    if isinstance(extra, list):
        lex |= {str(w).strip().lower() for w in extra if str(w).strip()}
    return lex


class ProfanityDetector:
    """Whole-word, case-insensitive lexicon matcher → PROFANITY spans.

    Implements the ``redact.Detector`` protocol so profane spans mask through the
    same ``type``/``char``/``hash`` modes as PII.
    """

    def __init__(self, lexicon: set[str]) -> None:
        self.lexicon = lexicon

    def detect(self, text: str, entities: list[str]) -> list[Span]:
        if "PROFANITY" not in entities:
            return []
        spans: list[Span] = []
        for m in re.finditer(r"[^\W_]+", text, re.UNICODE):
            if m.group(0).lower() in self.lexicon:
                spans.append(Span(m.start(), m.end(), "PROFANITY", m.group(0)))
        return spans


def _mask_profanity(text: str, lexicon: set[str], mask: str) -> tuple[str, int]:
    """Mask profane spans in ``text``; return (masked_text, count)."""
    spans = ProfanityDetector(lexicon).detect(text, ["PROFANITY"])
    if not spans:
        return text, 0
    out = text
    for s in sorted(spans, key=lambda s: s.start, reverse=True):
        out = out[: s.start] + _mask_for(s, mask) + out[s.end :]
    return out, len(spans)


@register_processor(
    "text.moderate",
    display_name="Content Moderation",
    description="Flag/mask profanity (lexicon) or score moderation categories (LLM).",
    tier="cpu_small",
    timeout_seconds=300,
    cost_per_job_usd=0.002,
    cacheable=False,
    model_id=_LLM_MODEL_ID,
    model_version_id=_LLM_MODEL_VERSION,
)
async def moderate_proc(ctx: dict[str, Any], job_id: str) -> dict[str, Any]:
    db = ctx["db"]
    job = db.fetchrow("SELECT org_id, artifact_id, params FROM jobs WHERE id = %s", job_id)
    if job is None:
        raise ValueError(f"job {job_id} not found")
    params = _params(job)
    transcript = _load_transcript(ctx, job, params)
    text = (transcript.get("text", "") or "")[:_MAX_INPUT_CHARS]
    engine = str(params.get("engine", "lexicon")).strip().lower()
    llm = get_llm()

    lexicon = _profanity_lexicon(params)
    mask = str(params.get("mask", "type"))
    masked_text, prof_count = _mask_profanity(text, lexicon, mask)
    profanity: dict[str, Any] = {"count": prof_count}
    if params.get("mask_profanity"):
        profanity["masked_text"] = masked_text

    result: dict[str, Any] = {
        "flagged": prof_count > 0,
        "profanity": profanity,
        "engine": engine,
        "model_version_id": llm.model_version_id,
    }

    if engine == "llm":
        # PRD 08: redact PII before the (possibly external) LLM sees the text.
        maybe_redact(transcript, params)
        safe_text = (transcript.get("text", "") or "")[:_MAX_INPUT_CHARS]
        data = _analyze_json(
            llm,
            _ANALYSIS_SYSTEM,
            'Return {"categories":{"hate":0-1,"harassment":0-1,"sexual":0-1,'
            '"violence":0-1,"self_harm":0-1},"flagged_spans":[short quoted phrases]}. '
            "Score how strongly the transcript expresses each moderation category.\n"
            f"<transcript>\n{safe_text}\n</transcript>",
            max_tokens=400,
        )
        if isinstance(data, dict) and isinstance(data.get("categories"), dict):
            cats = {k: float(v) for k, v in data["categories"].items() if _isnum(v)}
            result["categories"] = cats
            result["flagged_spans"] = data.get("flagged_spans", [])
            result["flagged"] = result["flagged"] or any(v >= 0.5 for v in cats.values())
        else:
            # LLM failed → fall back to lexicon-only signal (already computed).
            result.setdefault("warnings", []).append("llm_moderation_unavailable")
    return result


def _isnum(v: Any) -> bool:
    try:
        float(v)
        return True
    except (TypeError, ValueError):
        return False


# --- audio PII beep redaction (§4.5) ----------------------------------------


def _word_char_offsets(text: str, words: list[dict[str, Any]]) -> list[tuple[int, int]]:
    """Best-effort char span of each word within the segment text (handles
    arbitrary whitespace by scanning forward)."""
    offsets: list[tuple[int, int]] = []
    cur = 0
    for w in words:
        wt = str(w.get("word", "")).strip()
        if not wt:
            offsets.append((cur, cur))
            continue
        idx = text.find(wt, cur)
        if idx < 0:
            idx = cur
        offsets.append((idx, idx + len(wt)))
        cur = idx + len(wt)
    return offsets


# Guard band added to every muted PII range. ffmpeg's ``volume=…:enable`` gates
# per audio frame, so the frame straddling a range's start edge is only partially
# muted (~50-100ms can leak). Padding guarantees the PII word is fully covered —
# over-muting a sliver of neighbouring audio is the safe trade for redaction.
_PII_MUTE_PAD = 0.12


def _pii_time_ranges(
    transcript: dict[str, Any], detector: Any, entities: list[str], pad: float = _PII_MUTE_PAD
) -> tuple[list[tuple[float, float]], dict[str, int], bool]:
    """Map PII spans in each segment to audio [start, end] time ranges + counts.

    Uses word timestamps when present (tight ranges, padded by a guard band);
    otherwise falls back to muting the whole segment that contains PII (coarser,
    but nothing leaks). Returns ``(merged_ranges, counts, used_coarse_fallback)``.
    """
    ranges: list[tuple[float, float]] = []
    counts: dict[str, int] = {}
    coarse = False
    for seg in transcript.get("segments") or []:
        text = str(seg.get("text", ""))
        spans = detector.detect(text, entities)
        if not spans:
            continue
        for s in spans:
            counts[s.entity] = counts.get(s.entity, 0) + 1
        words = seg.get("words") or []
        if words:
            offsets = _word_char_offsets(text, words)
            for span in spans:
                for (a, b), w in zip(offsets, words, strict=False):
                    if a < span.end and b > span.start:  # word overlaps the PII span
                        start = max(0.0, float(w.get("start", 0.0)) - pad)
                        ranges.append((start, float(w.get("end", 0.0)) + pad))
        else:
            coarse = True
            ranges.append((float(seg.get("start", 0.0)), float(seg.get("end", 0.0))))
    return _merge_ranges(ranges), counts, coarse


def _merge_ranges(ranges: list[tuple[float, float]]) -> list[tuple[float, float]]:
    """Sort + merge overlapping/adjacent time ranges (0.05s join tolerance)."""
    valid = sorted((s, e) for s, e in ranges if e > s)
    merged: list[tuple[float, float]] = []
    for s, e in valid:
        if merged and s <= merged[-1][1] + 0.05:
            merged[-1] = (merged[-1][0], max(merged[-1][1], e))
        else:
            merged.append((s, e))
    return merged


def _write_audio_artifact(ctx: dict[str, Any], org_id: str, job_id: str, local_path: str) -> str:
    """Upload the redacted audio and register it as a tenant-scoped artifact."""
    s3 = ctx["s3"]
    bucket = ctx["bucket"]
    data = Path(local_path).read_bytes()
    key = f"redacted-audio/{org_id}/{job_id}.wav"
    s3.upload_file(bucket, key, local_path, "audio/wav")
    sha = hashlib.sha256(data).hexdigest()
    row = ctx["db"].fetchrow(
        """
        INSERT INTO artifacts (org_id, s3_bucket, s3_key, sha256, size_bytes,
            content_type, probe_status)
        VALUES (%s,%s,%s,%s,%s,'audio/wav','completed'::probe_status)
        RETURNING id::text
        """,
        org_id,
        bucket,
        key,
        sha,
        len(data),
    )
    return row["id"]


@register_processor(
    "audio.redact",
    display_name="Audio PII Redaction",
    description="Mute PII spans in the media (beep/silence) + return a redacted audio artifact.",
    tier="cpu_medium",
    timeout_seconds=900,
    cost_per_job_usd=0.01,
    cacheable=False,
    model_id="orpheus-audio-redact",
    model_version_id="orpheus-audio-redact-1",
)
async def audio_redact_proc(ctx: dict[str, Any], job_id: str) -> dict[str, Any]:
    db = ctx["db"]
    s3 = ctx["s3"]
    work_dir = Path(ctx["work_dir"])
    job = db.fetchrow("SELECT org_id, artifact_id, params FROM jobs WHERE id = %s", job_id)
    if job is None:
        raise ValueError(f"job {job_id} not found")
    params = _params(job)
    transcript = _load_transcript(ctx, job, params)

    entities = params.get("entities") or DEFAULT_ENTITIES
    ranges, counts, coarse = _pii_time_ranges(transcript, get_detector(), entities)
    warnings: list[str] = []
    if coarse:
        warnings.append("no_word_timestamps_muted_whole_segments")

    artifact_id = job["artifact_id"] or params.get("artifact_id")
    if not artifact_id:
        raise ValueError("audio.redact requires the source audio artifact")
    art = db.fetchrow(
        "SELECT s3_bucket, s3_key FROM artifacts WHERE id = %s AND org_id = %s",
        artifact_id,
        job["org_id"],
    )
    if art is None:
        raise ValueError(f"artifact {artifact_id} not found")

    work_dir.mkdir(parents=True, exist_ok=True)
    suffix = Path(art["s3_key"]).suffix or ".bin"
    src = work_dir / f"redact-{job_id}.src{suffix}"
    out = work_dir / f"redact-{job_id}.wav"
    try:
        s3.download_file(art["s3_bucket"], art["s3_key"], str(src))
        mute_ranges(src, out, ranges)
        redacted_artifact_id = _write_audio_artifact(ctx, job["org_id"], job_id, str(out))
    finally:
        for p in (src, out):
            p.unlink(missing_ok=True)

    result: dict[str, Any] = {
        "redacted_audio_artifact_id": redacted_artifact_id,
        "redactions": [{"entity": k, "count": v} for k, v in sorted(counts.items())],
        "muted_ranges": [{"start": round(s, 3), "end": round(e, 3)} for s, e in ranges],
        "model_version_id": "orpheus-audio-redact-1",
    }
    if warnings:
        result["warnings"] = warnings
    return result


# --- emotion + audio events (SenseVoice) — §4.1-4.3 -------------------------


def _fetch_source_wav(ctx: dict[str, Any], job: dict, params: dict, tag: str) -> Path:
    """Download the source media artifact and downmix to a 16kHz mono wav.

    Returns the wav path (caller cleans up its parent work_dir entries).
    """
    db = ctx["db"]
    s3 = ctx["s3"]
    work_dir = Path(ctx["work_dir"])
    artifact_id = job["artifact_id"] or params.get("artifact_id")
    if not artifact_id:
        raise ValueError(f"{tag} requires the source audio artifact")
    art = db.fetchrow(
        "SELECT s3_bucket, s3_key FROM artifacts WHERE id = %s AND org_id = %s",
        artifact_id,
        job["org_id"],
    )
    if art is None:
        raise ValueError(f"artifact {artifact_id} not found")
    work_dir.mkdir(parents=True, exist_ok=True)
    suffix = Path(art["s3_key"]).suffix or ".bin"
    src = work_dir / f"{tag}-{job['id'] if 'id' in job else 'j'}.src{suffix}"
    wav = work_dir / f"{tag}-src.wav"
    s3.download_file(art["s3_bucket"], art["s3_key"], str(src))
    try:
        convert_to_wav_16k_mono(src, wav)
    finally:
        src.unlink(missing_ok=True)
    return wav


def _run_senses(
    ctx: dict[str, Any], job_id: str, tag: str
) -> tuple[Any, list[dict[str, Any]], list[dict[str, Any]], list[str]]:
    """Shared prep for the SER/AED processors: resolve transcript + source audio,
    run the analyzer (degrading to the stub on any failure), and return
    ``(analyzer, segments, analyzed, warnings)``."""
    db = ctx["db"]
    job = db.fetchrow("SELECT id, org_id, artifact_id, params FROM jobs WHERE id = %s", job_id)
    if job is None:
        raise ValueError(f"job {job_id} not found")
    params = _params(job)
    transcript = _load_transcript(ctx, job, params)
    segments = [dict(s) for s in (transcript.get("segments") or [])]
    wav = _fetch_source_wav(ctx, job, params, tag)
    analyzer = get_sense_analyzer()
    warnings: list[str] = []
    try:
        analyzed = analyzer.analyze(wav, segments)
    except Exception as e:  # Modal unreachable/timeout/non-200 → neutral labels
        logger.warning("worker.senses_failed", job_id=job_id, err=str(e))
        analyzer = StubSenseAnalyzer()
        analyzed = analyzer.analyze(wav, segments)
        warnings.append("sense_service_unavailable")
    finally:
        wav.unlink(missing_ok=True)
    return analyzer, segments, analyzed, warnings


@register_processor(
    "audio.emotion",
    display_name="Emotion Recognition",
    description="Per-segment speech emotion labels (SenseVoice).",
    tier="gpu_a10g",
    timeout_seconds=900,
    cost_per_job_usd=0.02,
    cacheable=False,
    model_id=_SENSE_MODEL_ID,
    model_version_id=_SENSE_MODEL_VERSION,
)
async def emotion_proc(ctx: dict[str, Any], job_id: str) -> dict[str, Any]:
    analyzer, segments, analyzed, warnings = _run_senses(ctx, job_id, "emotion")
    out_segments: list[dict[str, Any]] = []
    dur_by_emotion: dict[str, float] = {}
    for seg, a in zip(segments, analyzed, strict=False):
        emotion = a.get("emotion", "neutral")
        out_segments.append(
            {
                "start": float(seg.get("start", 0.0)),
                "end": float(seg.get("end", 0.0)),
                "text": seg.get("text", ""),
                "emotion": emotion,
                "emotion_scores": a.get("emotion_scores", {}),
            }
        )
        dur_by_emotion[emotion] = dur_by_emotion.get(emotion, 0.0) + (
            float(seg.get("end", 0.0)) - float(seg.get("start", 0.0))
        )
    dominant = max(dur_by_emotion, key=lambda k: dur_by_emotion[k]) if dur_by_emotion else None
    result: dict[str, Any] = {
        "segments": out_segments,
        "dominant_emotion": dominant,
        "model_version_id": analyzer.model_version_id,
    }
    if warnings:
        result["warnings"] = warnings
    return result


@register_processor(
    "audio.events",
    display_name="Audio Event Detection",
    description="Detect audio events (laughter, music, applause, …) as time spans.",
    tier="gpu_a10g",
    timeout_seconds=900,
    cost_per_job_usd=0.02,
    cacheable=False,
    model_id=_SENSE_MODEL_ID,
    model_version_id=_SENSE_MODEL_VERSION,
)
async def events_proc(ctx: dict[str, Any], job_id: str) -> dict[str, Any]:
    analyzer, _segments, analyzed, warnings = _run_senses(ctx, job_id, "events")
    events: list[dict[str, Any]] = []
    summary: dict[str, int] = {}
    for a in analyzed:
        for ev in a.get("events", []) or []:
            label = str(ev.get("label", "")).strip()
            if not label:
                continue
            events.append(
                {
                    "start": float(a.get("start", 0.0)),
                    "end": float(a.get("end", 0.0)),
                    "label": label,
                    "confidence": float(ev.get("confidence", 0.0) or 0.0),
                }
            )
            summary[label] = summary.get(label, 0) + 1
    result: dict[str, Any] = {
        "events": events,
        "summary": summary,
        "model_version_id": analyzer.model_version_id,
    }
    if warnings:
        result["warnings"] = warnings
    return result
