"""Speech emotion + audio-event analysis provider (PRD 13 §4.1-4.3).

``get_sense_analyzer().analyze(wav_path, segments)`` returns per-segment emotion
and audio-event labels. ``ModalSenseAnalyzer`` offloads to the Modal GPU service
(``orpheus_senses.py``, SenseVoice-Small) when ``ORPHEUS_SENSE_BACKEND=modal`` +
``ORPHEUS_MODAL_SENSE_URL``/``ORPHEUS_MODAL_SENSE_TOKEN`` are set; otherwise the
dependency-free ``StubSenseAnalyzer`` runs (deterministic, for tests / CPU-only
deploys) — exactly the shape of ``diarize.get_diarizer()``.

Result shape (parallel to the input ``segments``):
    [{start, end, emotion, emotion_scores, events: [{label, confidence}]}]
"""

from __future__ import annotations

import base64
import os
from pathlib import Path
from typing import Protocol

# The emotion label space SenseVoice emits (mapped to lowercase on the wire).
EMOTIONS = ("neutral", "happy", "sad", "angry", "fearful", "disgusted", "surprised")


class SenseAnalyzer(Protocol):
    model_version_id: str

    def analyze(self, wav_path: str | Path, segments: list[dict]) -> list[dict]: ...


class StubSenseAnalyzer:
    """Deterministic, dependency-free fallback.

    Not a real SER/AED model — it labels every segment ``neutral`` / ``Speech`` so
    the processors, wiring, and result shape are exercised without funasr/torch.
    """

    model_version_id = "stub-senses-1"

    def analyze(self, wav_path: str | Path, segments: list[dict]) -> list[dict]:
        out = []
        for seg in segments or []:
            out.append(
                {
                    "start": float(seg.get("start", 0.0)),
                    "end": float(seg.get("end", 0.0)),
                    "emotion": "neutral",
                    "emotion_scores": {"neutral": 1.0},
                    "events": [{"label": "Speech", "confidence": 1.0}],
                }
            )
        return out


class ModalSenseAnalyzer:
    """Real GPU SER + AED via the Modal endpoint (SenseVoice-Small)."""

    model_version_id = "sensevoice-small-1"

    def __init__(self, url: str, token: str) -> None:
        self._url = url
        self._token = token

    def analyze(self, wav_path: str | Path, segments: list[dict]) -> list[dict]:
        from .modal_client import modal_post_json

        payload = {
            "token": self._token,
            "audio_b64": base64.b64encode(Path(wav_path).read_bytes()).decode(),
            "segments": [
                {"start": float(s.get("start", 0.0)), "end": float(s.get("end", 0.0))}
                for s in (segments or [])
            ],
        }
        data = modal_post_json(self._url, payload).json()
        self.model_version_id = data.get("model_version_id", self.model_version_id)
        return data.get("segments", [])


def _modal_sense_config() -> tuple[str, str] | None:
    if os.environ.get("ORPHEUS_SENSE_BACKEND", "").strip().lower() != "modal":
        return None
    url = os.environ.get("ORPHEUS_MODAL_SENSE_URL", "").strip()
    token = os.environ.get("ORPHEUS_MODAL_SENSE_TOKEN", "").strip()
    return (url, token) if url and token else None


def manifest_identity() -> tuple[str, str]:
    """(model_id, model_version_id) for the configured analyzer."""
    if _modal_sense_config():
        return "sensevoice", ModalSenseAnalyzer.model_version_id
    return "stub-senses", StubSenseAnalyzer.model_version_id


def get_sense_analyzer() -> SenseAnalyzer:
    """Return the Modal GPU analyzer when enabled + configured, else the stub."""
    cfg = _modal_sense_config()
    if cfg:
        return ModalSenseAnalyzer(cfg[0], cfg[1])
    return StubSenseAnalyzer()
