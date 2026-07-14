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
) -> dict:
    """Transcribe a 16kHz mono wav file.

    Returns ``{text, segments, language, duration_seconds}`` where
    ``segments`` is a list of ``{start, end, text}`` dicts, ``text``
    is the full transcript, and ``language`` is the detected
    language code.
    """
    model = _load_model(model_size, model_dir)
    try:
        segments_iter, info = model.transcribe(
            str(wav_path),
            beam_size=5,
            language="en",
        )
        segments = [
            {"start": seg.start, "end": seg.end, "text": seg.text.strip()} for seg in segments_iter
        ]
        text = " ".join(s["text"] for s in segments).strip()
        return {
            "text": text,
            "segments": segments,
            "language": info.language,
            "duration_seconds": float(info.duration),
        }
    except Exception as exc:
        raise TranscribeError(f"whisper transcribe failed: {exc}") from exc
