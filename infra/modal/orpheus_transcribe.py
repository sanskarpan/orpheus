"""Orpheus GPU transcription service on Modal.

Runs faster-whisper (CTranslate2) on a GPU with a modern multilingual model
(large-v3-turbo by default) — the counterpart to the worker's local CPU path.
The Orpheus worker calls the HTTPS endpoint below when
``ORPHEUS_WORKER_TRANSCRIBE_BACKEND=modal`` is set, sending audio + options and
receiving the same ``{text, segments, language, duration_seconds}`` shape (plus
``gpu_seconds`` for cost metering).

Deploy:  modal deploy infra/modal/orpheus_transcribe.py
Auth:    a shared secret (Modal Secret ``orpheus-modal-auth`` →
         ``ORPHEUS_MODAL_SHARED_SECRET``) checked against the ``X-Orpheus-Token``
         request header, so the GPU endpoint is not open to the world.

The CUDA+cuDNN base image is required for CTranslate2 GPU (it dlopen's
libcudnn); debian_slim ships no cuDNN. Model weights are cached on a Volume so
only the first cold start downloads them.
"""

from __future__ import annotations

import base64
import os
import tempfile
import time

import modal

APP_NAME = "orpheus-transcribe"
DEFAULT_MODEL = "large-v3-turbo"
CACHE_DIR = "/cache"

image = (
    modal.Image.from_registry(
        "nvidia/cuda:12.8.0-cudnn-devel-ubuntu22.04", add_python="3.12"
    )
    .apt_install("ffmpeg")
    .pip_install("faster-whisper==1.1.0", "requests", "fastapi[standard]")
    .env({"HF_HOME": CACHE_DIR, "HF_HUB_CACHE": CACHE_DIR})
)

app = modal.App(APP_NAME)
model_cache = modal.Volume.from_name("orpheus-whisper-cache", create_if_missing=True)
auth = modal.Secret.from_name("orpheus-modal-auth")


@app.cls(
    image=image,
    gpu="a10g",
    volumes={CACHE_DIR: model_cache},
    secrets=[auth],
    min_containers=0,          # scale to zero when idle → no standing GPU cost
    scaledown_window=300,      # keep a warm GPU for 5 min after the last request
    timeout=1800,
)
@modal.concurrent(max_inputs=4, target_inputs=2)  # a few concurrent decodes per GPU
class Transcriber:
    @modal.enter()
    def load(self) -> None:
        from faster_whisper import WhisperModel

        self._models: dict[str, WhisperModel] = {}
        # Warm the default model so the first request doesn't pay the download.
        self._get(DEFAULT_MODEL)

    def _get(self, model_size: str):
        from faster_whisper import WhisperModel

        m = self._models.get(model_size)
        if m is None:
            m = WhisperModel(
                model_size,
                device="cuda",
                compute_type="float16",
                download_root=CACHE_DIR,
            )
            self._models[model_size] = m
            model_cache.commit()  # persist newly downloaded weights
        return m

    @modal.method()
    def transcribe(self, payload: dict) -> dict:
        model_size = payload.get("model") or DEFAULT_MODEL
        language = payload.get("language") or None
        initial_prompt = payload.get("initial_prompt") or None
        vocabulary = payload.get("vocabulary") or None
        word_timestamps = bool(payload.get("word_timestamps", False))

        prompt_parts = []
        if vocabulary:
            terms = [t.strip() for t in vocabulary if isinstance(t, str) and t.strip()]
            if terms:
                prompt_parts.append(", ".join(terms) + ".")
        if initial_prompt:
            prompt_parts.append(initial_prompt.strip())
        prompt = " ".join(prompt_parts) or None

        audio_b64 = payload.get("audio_b64")
        if not audio_b64:
            raise ValueError("audio_b64 is required")
        raw = base64.b64decode(audio_b64)

        model = self._get(model_size)
        with tempfile.NamedTemporaryFile(suffix=".audio", delete=True) as f:
            f.write(raw)
            f.flush()
            t0 = time.monotonic()
            segments_iter, info = model.transcribe(
                f.name,
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
            gpu_seconds = time.monotonic() - t0

        text = " ".join(s["text"] for s in segments).strip()
        return {
            "text": text,
            "segments": segments,
            "language": info.language,
            "duration_seconds": float(info.duration),
            "model": model_size,
            "gpu_seconds": round(gpu_seconds, 4),
        }


@app.function(image=image, secrets=[auth])
@modal.fastapi_endpoint(method="POST")
def transcribe(payload: dict):
    """HTTPS entrypoint. Auth via a shared secret in ``payload['token']``.

    The token is checked against ``ORPHEUS_MODAL_SHARED_SECRET`` (injected from
    the ``orpheus-modal-auth`` Modal Secret), so the GPU endpoint rejects
    unauthenticated callers instead of letting anyone spend GPU time.
    """
    from fastapi import HTTPException  # imported in-container (image has fastapi)

    expected = os.environ.get("ORPHEUS_MODAL_SHARED_SECRET", "")
    token = payload.get("token", "") if isinstance(payload, dict) else ""
    if not expected or token != expected:
        raise HTTPException(status_code=401, detail="invalid or missing token")
    return Transcriber().transcribe.remote(payload)
