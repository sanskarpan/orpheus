"""Forced alignment: tighten word-level timestamps against the audio (PRD 01).

Whisper's word timestamps are approximate — they're derived from decoder
cross-attention, not from the acoustics. Forced alignment runs a character/
phoneme CTC model (wav2vec2 / MMS) over the audio *conditioned on the known
transcript*, producing tight, karaoke-accurate word boundaries suitable for
subtitles and precise clipping.

Backends (``ORPHEUS_ALIGN_BACKEND``):
  - ``modal`` — offload to the GPU endpoint (torchaudio ``MMS_FA``, multilingual,
    non-gated). Production default; scales to zero.
  - ``local`` — CPU torchaudio ``forced_align`` (heavy; opt-in for dev/offline).

``align_transcript(wav_path, transcript, language)`` mutates ``transcript`` in
place: each segment's ``words`` is replaced with aligned
``{word, start, end, confidence}`` in absolute seconds. Any failure raises
:class:`AlignError` — the caller decides whether to degrade to whisper timings.
"""

from __future__ import annotations

import base64
import os
import re
from pathlib import Path
from typing import Any

_WORD_RE = re.compile(r"[^\W_]+(?:['\u2019][^\W_]+)?", re.UNICODE)


class AlignError(Exception):
    pass


def _backend() -> str:
    return os.environ.get("ORPHEUS_ALIGN_BACKEND", "modal").strip().lower()


def _segment_tokens(text: str) -> list[str]:
    """Alignable tokens (letters/apostrophes) from a segment's text."""
    return _WORD_RE.findall(text or "")


def _apply_aligned_segments(
    transcript: dict[str, Any], aligned: list[dict[str, Any]]
) -> None:
    """Replace each segment's ``words`` with the aligned words (in place).

    ``aligned`` is parallel to ``transcript['segments']``; each entry carries
    ``words: [{word, start, end, confidence}]`` in absolute seconds. Segment
    ``start``/``end`` are tightened to the first/last aligned word when present.
    """
    segments = transcript.get("segments") or []
    for seg, al in zip(segments, aligned, strict=False):
        words = al.get("words") or []
        if not words:
            continue
        seg["words"] = [
            {
                "word": str(w.get("word", "")),
                "start": round(float(w.get("start", 0.0)), 3),
                "end": round(float(w.get("end", 0.0)), 3),
                "confidence": round(float(w.get("confidence", 0.0) or 0.0), 3),
            }
            for w in words
        ]
        seg["start"] = seg["words"][0]["start"]
        seg["end"] = seg["words"][-1]["end"]
    transcript["alignment"] = "forced"


def _align_modal(
    wav_path: str | Path, segments: list[dict[str, Any]], language: str | None
) -> list[dict[str, Any]]:
    """Offload alignment to the Modal GPU endpoint (torchaudio MMS_FA)."""
    from .modal_client import modal_post_json

    url = os.environ.get("ORPHEUS_MODAL_ALIGN_URL", "").strip()
    token = os.environ.get("ORPHEUS_MODAL_ALIGN_TOKEN", "").strip()
    if not url or not token:
        raise AlignError("modal align backend selected but ORPHEUS_MODAL_ALIGN_URL/TOKEN not set")
    payload = {
        "token": token,
        "audio_b64": base64.b64encode(Path(wav_path).read_bytes()).decode(),
        "language": language,
        "segments": [
            {
                "start": float(s.get("start", 0.0)),
                "end": float(s.get("end", 0.0)),
                "text": s.get("text", ""),
            }
            for s in segments
        ],
    }
    try:
        return modal_post_json(url, payload).json().get("segments", [])
    except Exception as exc:  # network / 4xx / 5xx
        raise AlignError(f"modal align failed: {exc}") from exc


def _align_local(
    wav_path: str | Path, segments: list[dict[str, Any]], language: str | None
) -> list[dict[str, Any]]:
    """CPU forced alignment via torchaudio's ``MMS_FA`` bundle.

    Aligns each segment's tokens within its [start, end] audio window and emits
    absolute word timings. Heavy (torch + torchaudio); imported lazily so the
    worker loads without them. Raises :class:`AlignError` if unavailable.
    """
    try:
        import torch
        import torchaudio
        from torchaudio.pipelines import MMS_FA as bundle
    except Exception as exc:  # pragma: no cover - optional heavy dep
        raise AlignError(f"local align backend requires torch/torchaudio: {exc}") from exc

    try:
        model = bundle.get_model()
        tokenizer = bundle.get_tokenizer()
        aligner = bundle.get_aligner()
        waveform, sr = torchaudio.load(str(wav_path))
        if waveform.shape[0] > 1:
            waveform = waveform.mean(dim=0, keepdim=True)
        target_sr = bundle.sample_rate
        if sr != target_sr:
            waveform = torchaudio.functional.resample(waveform, sr, target_sr)
            sr = target_sr

        out: list[dict[str, Any]] = []
        for seg in segments:
            tokens = _segment_tokens(seg.get("text", ""))
            start = float(seg.get("start", 0.0))
            end = float(seg.get("end", start))
            a, b = int(start * sr), int(end * sr)
            clip = waveform[:, a:b]
            if not tokens or clip.shape[1] < sr // 100:  # <10ms or no words
                out.append({"words": []})
                continue
            try:
                with torch.inference_mode():
                    emission, _ = model(clip)
                    token_spans = aligner(emission[0], tokenizer([t.lower() for t in tokens]))
                ratio = clip.shape[1] / emission.shape[1] / sr  # frames → seconds
                words = []
                for tok, spans in zip(tokens, token_spans, strict=False):
                    w_start = start + spans[0].start * ratio
                    w_end = start + spans[-1].end * ratio
                    score = sum(s.score * (s.end - s.start) for s in spans)
                    length = sum(s.end - s.start for s in spans) or 1
                    words.append(
                        {"word": tok, "start": w_start, "end": w_end, "confidence": score / length}
                    )
                out.append({"words": words})
            except Exception:
                # One segment failing to align (e.g. an OOV/numeric token) must not
                # sink the rest — degrade this segment to its original timings.
                out.append({"words": []})
        return out
    except AlignError:
        raise
    except Exception as exc:
        raise AlignError(f"local align failed: {exc}") from exc


def align_transcript(
    wav_path: str | Path, transcript: dict[str, Any], language: str | None = None
) -> dict[str, Any]:
    """Refine ``transcript['segments'][].words`` with forced alignment (in place)."""
    segments = transcript.get("segments") or []
    if not segments:
        return transcript
    backend = _backend()
    if backend == "modal":
        aligned = _align_modal(wav_path, segments, language)
    elif backend == "local":
        aligned = _align_local(wav_path, segments, language)
    else:
        raise AlignError(f"unknown align backend {backend!r}")
    _apply_aligned_segments(transcript, aligned)
    return transcript
