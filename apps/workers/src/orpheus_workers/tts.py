"""Text-to-speech provider (PRD 05) for the speech-to-speech loop.

``get_tts().synth(text, voice)`` returns ``(wav_bytes, sample_rate)``.
``ModalTTS`` offloads to the Kokoro Modal service when ``ORPHEUS_MODAL_TTS_URL`` +
``ORPHEUS_MODAL_TTS_TOKEN`` are set; otherwise ``StubTTS`` synthesizes a short
silent wav so the conversation loop is testable without GPU.
"""

from __future__ import annotations

import base64
import io
import os
import wave
from typing import Any, Protocol


class TTS(Protocol):
    model_version_id: str

    def synth(self, text: str, voice: str | None = None) -> tuple[bytes, int]: ...


class StubTTS:
    """Deterministic, dependency-free TTS: a silent wav whose length scales with
    the text, so the S2S loop and its wiring are testable without a model."""

    model_version_id = "stub-tts-1"
    sample_rate = 24000

    def synth(self, text: str, voice: str | None = None) -> tuple[bytes, int]:
        seconds = max(0.2, min(10.0, len(text) * 0.06))
        n = int(seconds * self.sample_rate)
        buf = io.BytesIO()
        with wave.open(buf, "wb") as w:
            w.setnchannels(1)
            w.setsampwidth(2)
            w.setframerate(self.sample_rate)
            w.writeframes(b"\x00\x00" * n)
        return buf.getvalue(), self.sample_rate


class ModalTTS:
    """Real neural TTS via the Kokoro Modal endpoint."""

    model_version_id = "kokoro-82m-1"

    def __init__(self, url: str, token: str) -> None:
        self._url = url
        self._token = token

    def synth(self, text: str, voice: str | None = None) -> tuple[bytes, int]:
        from .modal_client import modal_post_json

        payload: dict[str, Any] = {"token": self._token, "text": text}
        if voice:
            payload["voice"] = voice
        data = modal_post_json(self._url, payload).json()
        audio_b64 = data.get("audio_b64") or ""
        self.model_version_id = data.get("model_version_id", self.model_version_id)
        return base64.b64decode(audio_b64), int(data.get("sample_rate", 24000))


def _modal_tts_config() -> tuple[str, str] | None:
    url = os.environ.get("ORPHEUS_MODAL_TTS_URL", "").strip()
    token = os.environ.get("ORPHEUS_MODAL_TTS_TOKEN", "").strip()
    return (url, token) if url and token else None


def get_tts() -> TTS:
    cfg = _modal_tts_config()
    if cfg:
        return ModalTTS(cfg[0], cfg[1])
    return StubTTS()
