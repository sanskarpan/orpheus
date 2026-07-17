"""Text processors over transcripts (PRD 04): detect-language, translate, summarize.

Each accepts either a ``source_job_id`` (a prior job whose result is the
transcript) or an ``artifact_id`` (a transcript.json artifact). Output follows
the standard result shape and records the ``model_version_id`` that produced
it. The LLM is pluggable (see ``llm.py``): a deterministic stub runs tests and
key-less deployments, Claude runs when configured.
"""

from __future__ import annotations

import json
from pathlib import Path
from typing import Any

from ..llm import get_llm
from ..redact import maybe_redact
from . import register_processor

# Abuse guard: cap transcript length fed to the LLM.
_MAX_INPUT_CHARS = 100_000


def _params(job: dict) -> dict:
    params = job["params"] or {}
    if isinstance(params, str):
        params = json.loads(params)
    return params


def _load_transcript(ctx: dict[str, Any], job: dict, params: dict) -> dict:
    """Resolve the input transcript from source_job_id, an artifact param, or
    the job's own artifact. Returns a ``{text, segments, language, ...}`` dict."""
    db = ctx["db"]
    src_job = params.get("source_job_id")
    if src_job:
        row = db.fetchrow("SELECT result FROM jobs WHERE id = %s", src_job)
        if row and row["result"]:
            r = row["result"]
            return r if isinstance(r, dict) else json.loads(r)

    art_id = params.get("artifact_id") or job.get("artifact_id")
    if art_id:
        art = db.fetchrow("SELECT s3_bucket, s3_key FROM artifacts WHERE id = %s", art_id)
        if art is not None:
            work = Path(ctx["work_dir"])
            work.mkdir(parents=True, exist_ok=True)
            local = work / f"transcript-{art_id}.json"
            ctx["s3"].download_file(art["s3_bucket"], art["s3_key"], str(local))
            try:
                data = json.loads(local.read_text())
            finally:
                local.unlink(missing_ok=True)
            if isinstance(data, dict):
                return data
    raise ValueError("no transcript source (set params.source_job_id or params.artifact_id)")


@register_processor("text.detect-language")
async def detect_language_proc(ctx: dict[str, Any], job_id: str) -> dict[str, Any]:
    db = ctx["db"]
    job = db.fetchrow("SELECT org_id, artifact_id, params FROM jobs WHERE id = %s", job_id)
    if job is None:
        raise ValueError(f"job {job_id} not found")
    params = _params(job)
    transcript = _load_transcript(ctx, job, params)

    # Prefer Whisper's own detection when present (cheap + already done).
    lang = transcript.get("language")
    if lang:
        return {
            "language": lang,
            "confidence": float(transcript.get("language_probability", 1.0)),
            "model_version_id": "whisper-detect",
        }
    llm = get_llm()
    code, conf = llm.detect_language(transcript.get("text", ""))
    return {"language": code, "confidence": conf, "model_version_id": llm.model_version_id}


@register_processor("text.translate")
async def translate_proc(ctx: dict[str, Any], job_id: str) -> dict[str, Any]:
    db = ctx["db"]
    job = db.fetchrow("SELECT org_id, artifact_id, params FROM jobs WHERE id = %s", job_id)
    if job is None:
        raise ValueError(f"job {job_id} not found")
    params = _params(job)
    target = params.get("target_language")
    if not target:
        raise ValueError("params.target_language required")
    source = params.get("source_language", "auto")
    transcript = _load_transcript(ctx, job, params)
    # PRD 08: redact PII before translation so downstream never sees it.
    maybe_redact(transcript, params)
    llm = get_llm()

    out_segments: list[dict] = []
    for seg in transcript.get("segments", []) or []:
        translated = llm.translate(seg.get("text", ""), target, source)
        ns = {"start": seg.get("start"), "end": seg.get("end"), "text": translated}
        if "speaker" in seg:
            ns["speaker"] = seg["speaker"]
        out_segments.append(ns)

    if out_segments:
        full = " ".join(s["text"] for s in out_segments).strip()
    else:
        full = llm.translate(transcript.get("text", ""), target, source)
    return {
        "segments": out_segments,
        "text": full,
        "target_language": target,
        "source_language": source,
        "model_version_id": llm.model_version_id,
    }


@register_processor("text.summarize")
async def summarize_proc(ctx: dict[str, Any], job_id: str) -> dict[str, Any]:
    db = ctx["db"]
    job = db.fetchrow("SELECT org_id, artifact_id, params FROM jobs WHERE id = %s", job_id)
    if job is None:
        raise ValueError(f"job {job_id} not found")
    params = _params(job)
    transcript = _load_transcript(ctx, job, params)
    # PRD 08: redact PII before the (possibly external) LLM sees the text.
    maybe_redact(transcript, params)
    text = (transcript.get("text", "") or "")[:_MAX_INPUT_CHARS]
    if not text.strip():
        raise ValueError("transcript has no text to summarize")

    mode = params.get("mode", "abstract")
    max_tokens = int(params.get("max_tokens", 512))
    language = params.get("language", "en")
    llm = get_llm()
    summary = llm.summarize(text, mode=mode, max_tokens=max_tokens, language=language)
    return {
        "summary": summary,
        "mode": mode,
        "language": language,
        "model_version_id": llm.model_version_id,
    }
