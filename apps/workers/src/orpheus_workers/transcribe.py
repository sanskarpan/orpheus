"""Whisper transcription via faster-whisper."""

from __future__ import annotations

import os
from pathlib import Path

from faster_whisper import WhisperModel

_model: WhisperModel | None = None


class TranscribeError(Exception):
    pass


def _load_model(model_size: str, model_dir: str | None) -> WhisperModel:
    global _model
    if _model is None:
        if model_dir is None:
            model_dir = os.environ.get("ORPHEUS_WORKER_WHISPER_DIR")
        try:
            _model = WhisperModel(model_size, download_root=model_dir)
        except Exception as exc:
            raise TranscribeError(
                f"failed to load whisper model {model_size!r} from {model_dir!r}: {exc}"
            ) from exc
    return _model


def transcribe(
    wav_path: str | Path,
    model_size: str = "tiny.en",
    model_dir: str | None = None,
    word_timestamps: bool = False,
) -> dict:
    """Transcribe a 16kHz mono wav file.

    Returns ``{text, segments, language, duration_seconds}`` where
    ``segments`` is a list of ``{start, end, text}`` dicts, ``text``
    is the full transcript, and ``language`` is the detected
    language code. When ``word_timestamps`` is set, each segment also
    carries ``words = [{start, end, word, confidence}]`` (PRD 05).
    """
    model = _load_model(model_size, model_dir)
    try:
        segments_iter, info = model.transcribe(
            str(wav_path),
            beam_size=5,
            language="en",
            word_timestamps=word_timestamps,
        )
        segments = []
        for seg in segments_iter:
            entry = {"start": seg.start, "end": seg.end, "text": seg.text.strip()}
            words = getattr(seg, "words", None) if word_timestamps else None
            if words:
                entry["words"] = [
                    {
                        "start": w.start,
                        "end": w.end,
                        "word": w.word.strip(),
                        "confidence": float(getattr(w, "probability", 0.0) or 0.0),
                    }
                    for w in words
                ]
            segments.append(entry)
        text = " ".join(s["text"] for s in segments).strip()
        return {
            "text": text,
            "segments": segments,
            "language": info.language,
            "duration_seconds": float(info.duration),
        }
    except Exception as exc:
        raise TranscribeError(f"whisper transcribe failed: {exc}") from exc
