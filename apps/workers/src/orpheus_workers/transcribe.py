"""Whisper transcription via faster-whisper.

Model selection and hardware placement are configurable rather than baked in:

- ``model_size``  — any faster-whisper/CTranslate2 model (``tiny.en`` .. ``large-v3``,
  ``large-v3-turbo``, ``distil-large-v3``); default from ``ORPHEUS_WORKER_WHISPER_MODEL``.
- ``device``      — ``auto`` (default) resolves to CUDA when present, else CPU;
  override with ``ORPHEUS_WORKER_WHISPER_DEVICE``.
- ``compute_type``— ``int8`` by default (fast + low-memory on CPU, valid on GPU);
  override with ``ORPHEUS_WORKER_WHISPER_COMPUTE`` (e.g. ``float16`` on a real GPU).

Language is auto-detected by default (``language=None``); callers may force a
language code. A model is loaded once per distinct (size, dir, device, compute)
tuple and cached, so per-request model selection does not thrash a single global.
"""

from __future__ import annotations

import base64
import os
from pathlib import Path

from faster_whisper import WhisperModel

# Cache keyed by (model_size, model_dir, device, compute_type) so switching
# models per request reuses a warm instance instead of clobbering one global.
_models: dict[tuple[str, str | None, str, str], WhisperModel] = {}


class TranscribeError(Exception):
    pass


def _backend() -> str:
    """Transcription backend: 'local' (CPU faster-whisper) or 'modal' (GPU)."""
    return os.environ.get("ORPHEUS_WORKER_TRANSCRIBE_BACKEND", "local").strip().lower()


def _transcribe_modal(
    wav_path: str | Path,
    model_size: str,
    word_timestamps: bool,
    language: str | None,
    initial_prompt: str | None,
    vocabulary: list[str] | None,
) -> dict:
    """Offload transcription to the Modal GPU endpoint (large-v3-turbo, float16).

    Sends the audio + options and returns the same shape as the local path,
    plus ``gpu_seconds`` for cost metering. A job-specified model is honored;
    otherwise the endpoint's GPU default (large-v3-turbo) is used rather than
    the local CPU default (tiny.en).
    """
    from .modal_client import modal_post_json

    url = os.environ.get("ORPHEUS_MODAL_TRANSCRIBE_URL", "").strip()
    token = os.environ.get("ORPHEUS_MODAL_TRANSCRIBE_TOKEN", "").strip()
    if not url or not token:
        raise TranscribeError(
            "modal backend selected but ORPHEUS_MODAL_TRANSCRIBE_URL/TOKEN not set"
        )
    local_default = os.environ.get("ORPHEUS_WORKER_WHISPER_MODEL", "tiny.en")
    payload = {
        "token": token,
        "audio_b64": base64.b64encode(Path(wav_path).read_bytes()).decode(),
        # Only forward an explicit, non-default model; else let the GPU default win.
        "model": model_size if model_size and model_size != local_default else None,
        "language": language,
        "initial_prompt": initial_prompt,
        "vocabulary": vocabulary,
        "word_timestamps": word_timestamps,
    }
    try:
        return modal_post_json(url, payload).json()
    except Exception as exc:
        raise TranscribeError(f"modal transcribe failed: {exc}") from exc


def _default_device() -> str:
    return os.environ.get("ORPHEUS_WORKER_WHISPER_DEVICE", "auto")


def _default_compute_type() -> str:
    # int8 is a large CPU speed/memory win over the float32 default and remains
    # valid on CUDA; deployments with a real GPU can set float16 via env.
    return os.environ.get("ORPHEUS_WORKER_WHISPER_COMPUTE", "int8")


def _load_model(
    model_size: str,
    model_dir: str | None,
    device: str | None = None,
    compute_type: str | None = None,
) -> WhisperModel:
    if model_dir is None:
        model_dir = os.environ.get("ORPHEUS_WORKER_WHISPER_DIR") or None
    device = device or _default_device()
    compute_type = compute_type or _default_compute_type()
    key = (model_size, model_dir, device, compute_type)
    model = _models.get(key)
    if model is None:
        try:
            model = WhisperModel(
                model_size,
                download_root=model_dir,
                device=device,
                compute_type=compute_type,
            )
        except Exception as exc:
            raise TranscribeError(
                f"failed to load whisper model {model_size!r} (device={device!r}, "
                f"compute_type={compute_type!r}) from {model_dir!r}: {exc}"
            ) from exc
        _models[key] = model
    return model


def warmup(
    model_size: str | None = None,
    model_dir: str | None = None,
    device: str | None = None,
    compute_type: str | None = None,
) -> None:
    """Preload the default (or given) model so the first job doesn't pay cold start."""
    if _backend() == "modal":
        return  # GPU model is warmed remotely by the Modal container
    model_size = model_size or os.environ.get("ORPHEUS_WORKER_WHISPER_MODEL", "tiny.en")
    _load_model(model_size, model_dir, device, compute_type)


def _build_initial_prompt(
    initial_prompt: str | None,
    vocabulary: list[str] | None,
) -> str | None:
    """Combine an explicit prompt with a custom-vocabulary list for keyterm biasing.

    faster-whisper conditions decoding on ``initial_prompt``; seeding it with
    domain terms nudges the model toward the intended spellings.
    """
    parts: list[str] = []
    if vocabulary:
        terms = [t.strip() for t in vocabulary if isinstance(t, str) and t.strip()]
        if terms:
            parts.append(", ".join(terms) + ".")
    if initial_prompt and initial_prompt.strip():
        parts.append(initial_prompt.strip())
    if not parts:
        return None
    return " ".join(parts)


def transcribe(
    wav_path: str | Path,
    model_size: str = "tiny.en",
    model_dir: str | None = None,
    word_timestamps: bool = False,
    language: str | None = None,
    initial_prompt: str | None = None,
    vocabulary: list[str] | None = None,
    device: str | None = None,
    compute_type: str | None = None,
) -> dict:
    """Transcribe a 16kHz mono wav file.

    Returns ``{text, segments, language, duration_seconds}`` where
    ``segments`` is a list of ``{start, end, text}`` dicts, ``text``
    is the full transcript, and ``language`` is the detected (or
    requested) language code. When ``word_timestamps`` is set, each
    segment also carries ``words = [{start, end, word, confidence}]``
    (PRD 05).

    ``language`` defaults to ``None`` (auto-detect); pass a code (e.g.
    ``"fr"``) to force it. ``vocabulary``/``initial_prompt`` bias
    recognition toward domain terms.

    When ``ORPHEUS_WORKER_TRANSCRIBE_BACKEND=modal``, the work is offloaded to
    the Modal GPU endpoint instead of the local CPU model.
    """
    if _backend() == "modal":
        return _transcribe_modal(
            wav_path,
            model_size=model_size,
            word_timestamps=word_timestamps,
            language=language,
            initial_prompt=initial_prompt,
            vocabulary=vocabulary,
        )
    model = _load_model(model_size, model_dir, device, compute_type)
    prompt = _build_initial_prompt(initial_prompt, vocabulary)
    try:
        segments_iter, info = model.transcribe(
            str(wav_path),
            beam_size=5,
            language=language,
            initial_prompt=prompt,
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
