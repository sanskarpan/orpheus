"""Orpheus text-to-speech service on Modal GPU (PRD 15 §speech-to-speech).

Runs **Kokoro-82M** (hexgrad, Apache-2.0, non-gated) — a small, high-quality
neural TTS — behind the same shared-secret HTTPS pattern as the other Modal apps.
Powers the TTS leg of the full-duplex speech-to-speech loop (ASR → LLM → TTS).

Endpoint: POST JSON ``{token, text, voice?, speed?}`` → ``{audio_b64 (24 kHz mono
wav), sample_rate, gpu_seconds}``. Auth is the shared secret in
``orpheus-modal-auth``. Selected by the worker via ``ORPHEUS_MODAL_TTS_URL`` +
``ORPHEUS_MODAL_TTS_TOKEN``.

Deploy: modal deploy infra/modal/orpheus_tts.py
"""

from __future__ import annotations

import modal

APP_NAME = "orpheus-tts"
CACHE_DIR = "/cache"
MODEL_VERSION_ID = "kokoro-82m-1"
SAMPLE_RATE = 24000
DEFAULT_VOICE = "af_heart"

image = (
    modal.Image.debian_slim(python_version="3.12")
    .apt_install("espeak-ng")  # Kokoro's phonemizer backend
    .pip_install(
        "kokoro==0.9.4",
        "soundfile==0.12.1",
        "numpy<2",
        "torch==2.5.1",
        "fastapi[standard]",
    )
    .env({"HF_HOME": CACHE_DIR})
)

app = modal.App(APP_NAME)
model_cache = modal.Volume.from_name("orpheus-tts-cache", create_if_missing=True)
auth = modal.Secret.from_name("orpheus-modal-auth")


@app.cls(
    image=image,
    gpu="a10g",
    volumes={CACHE_DIR: model_cache},
    secrets=[auth],
    min_containers=0,
    scaledown_window=300,
    timeout=900,
)
@modal.concurrent(max_inputs=4)
class TTS:
    @modal.enter()
    def load(self):
        from kokoro import KPipeline

        # lang_code 'a' = American English; the pipeline lazy-loads voices.
        self.pipeline = KPipeline(lang_code="a")
        model_cache.commit()

    @modal.method()
    def synth(self, payload: dict) -> dict:
        import base64
        import io
        import time

        import numpy as np
        import soundfile as sf

        t0 = time.monotonic()
        text = str(payload.get("text", "")).strip()
        voice = str(payload.get("voice") or DEFAULT_VOICE)
        speed = float(payload.get("speed") or 1.0)
        if not text:
            return {
                "audio_b64": "",
                "sample_rate": SAMPLE_RATE,
                "model_version_id": MODEL_VERSION_ID,
                "gpu_seconds": 0.0,
            }

        chunks = []
        for _graphemes, _phonemes, audio in self.pipeline(text, voice=voice, speed=speed):
            arr = audio.detach().cpu().numpy() if hasattr(audio, "detach") else np.asarray(audio)
            chunks.append(arr.astype(np.float32))
        wav = np.concatenate(chunks) if chunks else np.zeros(1, dtype=np.float32)

        buf = io.BytesIO()
        sf.write(buf, wav, SAMPLE_RATE, format="WAV", subtype="PCM_16")
        return {
            "audio_b64": base64.b64encode(buf.getvalue()).decode(),
            "sample_rate": SAMPLE_RATE,
            "duration_seconds": round(len(wav) / SAMPLE_RATE, 3),
            "voice": voice,
            "model_version_id": MODEL_VERSION_ID,
            "gpu_seconds": round(time.monotonic() - t0, 4),
        }


@app.function(image=image, secrets=[auth], timeout=900)
@modal.fastapi_endpoint(method="POST")
def synth(payload: dict):
    import os

    from fastapi import HTTPException

    expected = os.environ.get("ORPHEUS_MODAL_SHARED_SECRET", "")
    if not expected or (payload.get("token", "") if isinstance(payload, dict) else "") != expected:
        raise HTTPException(status_code=401, detail="invalid or missing token")
    return TTS().synth.remote(payload)
