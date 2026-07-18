"""Audio enrichment processors (PRD 05): diarization + subtitle export.

- ``audio.diarize`` assigns anonymous speaker labels (S1..Sn) to transcript
  segments by overlapping the transcript with diarization turns (pyannote when
  configured, else a deterministic stub).
- ``export.subtitles`` renders ``.srt``/``.vtt`` from a transcript (with
  optional speaker labels, line wrapping) and stores them as artifacts.

The subtitle builders are pure functions so they are unit-testable without a
DB/S3, and text is sanitized before being written into VTT (which allows
limited markup) to avoid caption-based injection in downstream players.
"""

from __future__ import annotations

import hashlib
import json
from pathlib import Path
from typing import Any

from ..diarize import get_diarizer
from ..ffmpeg import convert_to_wav_16k_mono
from . import register_processor


def _params(job: dict) -> dict:
    params = job["params"] or {}
    if isinstance(params, str):
        params = json.loads(params)
    return params


def _load_transcript(ctx: dict[str, Any], job: dict, params: dict) -> dict:
    db = ctx["db"]
    src_job = params.get("source_job_id")
    if src_job:
        row = db.fetchrow("SELECT result FROM jobs WHERE id = %s", src_job)
        if row and row["result"]:
            r = row["result"]
            return r if isinstance(r, dict) else json.loads(r)
    raise ValueError("no transcript source (set params.source_job_id)")


# --- subtitle builders (pure) -----------------------------------------------


def _fmt_ts(seconds: float, sep: str) -> str:
    if seconds < 0:
        seconds = 0.0
    ms = int(round(seconds * 1000))
    h, ms = divmod(ms, 3_600_000)
    m, ms = divmod(ms, 60_000)
    s, ms = divmod(ms, 1000)
    return f"{h:02d}:{m:02d}:{s:02d}{sep}{ms:03d}"


def _wrap(text: str, max_chars: int, max_lines: int) -> str:
    words = text.split()
    lines: list[str] = []
    cur = ""
    for w in words:
        if cur and len(cur) + 1 + len(w) > max_chars:
            lines.append(cur)
            cur = w
            if len(lines) >= max_lines:
                break
        else:
            cur = f"{cur} {w}".strip()
    if cur and len(lines) < max_lines:
        lines.append(cur)
    return "\n".join(lines) if lines else text[: max_chars * max_lines]


def _escape_vtt(text: str) -> str:
    # VTT permits a small markup subset; escape the chars that start it.
    return text.replace("&", "&amp;").replace("<", "&lt;").replace(">", "&gt;")


def _cue_text(seg: dict, include_speaker: bool) -> str:
    text = (seg.get("text") or "").strip()
    if include_speaker and seg.get("speaker"):
        text = f"{seg['speaker']}: {text}"
    return text


def build_srt(segments: list[dict], include_speaker: bool, max_chars: int, max_lines: int) -> str:
    blocks: list[str] = []
    for i, seg in enumerate(segments, start=1):
        text = _wrap(_cue_text(seg, include_speaker), max_chars, max_lines)
        blocks.append(
            f"{i}\n{_fmt_ts(seg.get('start', 0.0), ',')} --> {_fmt_ts(seg.get('end', 0.0), ',')}\n{text}\n"
        )
    return "\n".join(blocks)


def build_vtt(segments: list[dict], include_speaker: bool, max_chars: int, max_lines: int) -> str:
    out = ["WEBVTT", ""]
    for seg in segments:
        text = _wrap(_escape_vtt(_cue_text(seg, include_speaker)), max_chars, max_lines)
        out.append(f"{_fmt_ts(seg.get('start', 0.0), '.')} --> {_fmt_ts(seg.get('end', 0.0), '.')}")
        out.append(text)
        out.append("")
    return "\n".join(out)


# --- processors --------------------------------------------------------------


def _overlap_speaker(turns: list[dict], seg: dict) -> str | None:
    s0, s1 = float(seg.get("start", 0.0)), float(seg.get("end", 0.0))
    best, best_ov = None, 0.0
    for t in turns:
        ov = min(s1, t["end"]) - max(s0, t["start"])
        if ov > best_ov:
            best_ov, best = ov, t["speaker"]
    return best


@register_processor(
    "audio.diarize",
    display_name="Diarize",
    description="Assign anonymous speaker labels (S1..Sn) to transcript segments.",
    tier="cpu_medium",
    timeout_seconds=1800,
    cost_per_job_usd=0.01,
    model_id="pyannote",
    model_version_id="pyannote-1",
)
async def diarize_proc(ctx: dict[str, Any], job_id: str) -> dict[str, Any]:
    db = ctx["db"]
    s3 = ctx["s3"]
    work_dir = Path(ctx["work_dir"])
    job = db.fetchrow("SELECT org_id, artifact_id, params FROM jobs WHERE id = %s", job_id)
    if job is None:
        raise ValueError(f"job {job_id} not found")
    params = _params(job)
    transcript = _load_transcript(ctx, job, params)

    artifact_id = job["artifact_id"] or params.get("artifact_id")
    if not artifact_id:
        raise ValueError("audio.diarize requires the source audio artifact")
    art = db.fetchrow("SELECT s3_bucket, s3_key FROM artifacts WHERE id = %s", artifact_id)
    if art is None:
        raise ValueError(f"artifact {artifact_id} not found")

    work_dir.mkdir(parents=True, exist_ok=True)
    suffix = Path(art["s3_key"]).suffix or ".bin"
    src = work_dir / f"diar-{job_id}.src{suffix}"
    wav = work_dir / f"diar-{job_id}.wav"
    try:
        s3.download_file(art["s3_bucket"], art["s3_key"], str(src))
        convert_to_wav_16k_mono(src, wav)
        diarizer = get_diarizer(num_speakers=int(params.get("max_speakers", 2)))
        turns = diarizer.diarize(wav)
    finally:
        for p in (src, wav):
            p.unlink(missing_ok=True)

    segments = [dict(seg) for seg in transcript.get("segments", []) or []]
    for seg in segments:
        spk = _overlap_speaker(turns, seg)
        if spk:
            seg["speaker"] = spk
    speakers = sorted({t["speaker"] for t in turns})
    return {
        "segments": segments,
        "text": transcript.get("text", ""),
        "language": transcript.get("language"),
        "speakers": speakers,
        "num_speakers": len(speakers),
        "model_version_id": diarizer.model_version_id,
    }


@register_processor(
    "export.subtitles",
    display_name="Export Subtitles",
    description="Render .srt/.vtt from a transcript with optional speaker labels.",
    tier="cpu_tiny",
    timeout_seconds=120,
    cost_per_job_usd=0.0005,
    model_id="subtitle-render",
    model_version_id="subtitle-1",
)
async def export_subtitles_proc(ctx: dict[str, Any], job_id: str) -> dict[str, Any]:
    db = ctx["db"]
    s3 = ctx["s3"]
    bucket = ctx["bucket"]
    work_dir = Path(ctx["work_dir"])
    job = db.fetchrow("SELECT org_id, artifact_id, params FROM jobs WHERE id = %s", job_id)
    if job is None:
        raise ValueError(f"job {job_id} not found")
    org_id = job["org_id"]
    params = _params(job)
    transcript = _load_transcript(ctx, job, params)
    segments = transcript.get("segments", []) or []

    formats = params.get("formats") or ["srt", "vtt"]
    include_speaker = bool(params.get("include_speaker_labels", True))
    max_chars = int(params.get("max_chars_per_line", 42))
    max_lines = int(params.get("max_lines", 2))

    renderers = {
        "srt": (build_srt, "application/x-subrip", ".srt"),
        "vtt": (build_vtt, "text/vtt", ".vtt"),
    }
    work_dir.mkdir(parents=True, exist_ok=True)
    outputs: list[dict] = []
    for fmt in formats:
        if fmt not in renderers:
            continue
        build, content_type, ext = renderers[fmt]
        content = build(segments, include_speaker, max_chars, max_lines)
        data = content.encode("utf-8")
        local = work_dir / f"subs-{job_id}{ext}"
        local.write_bytes(data)
        s3_key = f"subtitles/{org_id}/{job_id}{ext}"
        try:
            s3.upload_file(bucket, s3_key, str(local), content_type)
        finally:
            local.unlink(missing_ok=True)
        artifact_id = _insert_artifact(db, org_id, bucket, s3_key, data, content_type)
        outputs.append({"format": fmt, "artifact_id": artifact_id, "s3_key": s3_key})

    if not outputs:
        raise ValueError("no valid subtitle formats requested (use srt and/or vtt)")
    return {"formats": [o["format"] for o in outputs], "artifacts": outputs}


def _insert_artifact(db, org_id: str, bucket: str, key: str, data: bytes, content_type: str) -> str:
    sha = hashlib.sha256(data).hexdigest()
    row = db.fetchrow(
        """
        INSERT INTO artifacts (org_id, s3_bucket, s3_key, sha256, size_bytes, content_type, probe_status)
        VALUES (%s, %s, %s, %s, %s, %s, 'completed'::probe_status)
        RETURNING id::text
        """,
        org_id,
        bucket,
        key,
        sha,
        len(data),
        content_type,
    )
    return row["id"]
