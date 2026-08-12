"""Speaker diarization provider (PRD 05).

``diarize(wav_path) -> [{start, end, speaker}]`` speaker turns. ``PyannoteDiarizer``
is the real CPU implementation (pyannote.audio, installed via the optional
``diarize`` extra + a Hugging Face token). ``StubDiarizer`` is a deterministic,
dependency-free fallback for tests and deployments without the model. Selection
is via ``get_diarizer()``.

Labels are anonymous S1..Sn — no speaker identification / voiceprints.
"""

from __future__ import annotations

import os
import wave
from pathlib import Path
from typing import Protocol


class Diarizer(Protocol):
    model_version_id: str

    def diarize(self, wav_path: str | Path) -> list[dict]: ...


def _wav_duration(wav_path: str | Path) -> float:
    with wave.open(str(wav_path), "rb") as w:
        rate = w.getframerate()
        return w.getnframes() / float(rate) if rate else 0.0


class StubDiarizer:
    """Deterministic fallback: round-robin speakers over fixed windows.

    Not a real diarization model — it segments purely by time so the rest of
    the pipeline (assignment, subtitles) is exercised without pyannote/torch.
    """

    model_version_id = "stub-diarize-1"

    def __init__(self, window_seconds: float = 5.0, num_speakers: int = 2) -> None:
        self.window_seconds = window_seconds
        self.num_speakers = max(1, num_speakers)

    def diarize(self, wav_path: str | Path) -> list[dict]:
        duration = _wav_duration(wav_path)
        turns: list[dict] = []
        start = 0.0
        i = 0
        while start < duration:
            end = min(start + self.window_seconds, duration)
            turns.append({"start": start, "end": end, "speaker": f"S{i % self.num_speakers + 1}"})
            start = end
            i += 1
        if not turns:
            turns.append({"start": 0.0, "end": duration, "speaker": "S1"})
        return turns


class PyannoteDiarizer:
    """Real CPU diarization via pyannote.audio. Lazy-imported so the module
    loads without torch/pyannote installed."""

    def __init__(self, model: str, hf_token: str | None) -> None:
        from pyannote.audio import Pipeline  # type: ignore  # noqa: PLC0415  (optional heavy dep)

        self.model_version_id = f"pyannote:{model}"
        self._pipeline = Pipeline.from_pretrained(model, use_auth_token=hf_token)

    def diarize(self, wav_path: str | Path) -> list[dict]:
        annotation = self._pipeline(str(wav_path))
        labels: dict[str, str] = {}
        turns: list[dict] = []
        for segment, _, label in annotation.itertracks(yield_label=True):
            if label not in labels:
                labels[label] = f"S{len(labels) + 1}"
            turns.append(
                {"start": float(segment.start), "end": float(segment.end), "speaker": labels[label]}
            )
        turns.sort(key=lambda t: t["start"])
        return turns


class ModalDiarizer:
    """Real GPU diarization via the Modal endpoint (ECAPA embeddings + clustering).

    Sends the 16 kHz mono wav and returns ``[{start, end, speaker}]`` turns —
    genuine speaker attribution (the same speaker recurs across turns), unlike the
    round-robin stub.
    """

    model_version_id = "ecapa-agglomerative-1"

    def __init__(self, url: str, token: str, max_speakers: int = 6) -> None:
        self._url = url
        self._token = token
        self._max_speakers = max(int(max_speakers), 2)

    def diarize(self, wav_path):  # noqa: ANN001 - matches the Diarizer protocol
        import base64

        import httpx

        with open(wav_path, "rb") as f:
            audio = f.read()
        payload = {
            "token": self._token,
            "audio_b64": base64.b64encode(audio).decode(),
            "max_speakers": self._max_speakers,
        }
        resp = httpx.post(self._url, json=payload, timeout=600.0)
        resp.raise_for_status()
        data = resp.json()
        self.model_version_id = data.get("model_version_id", self.model_version_id)
        return data.get("turns", [])


def _modal_diarize_config() -> tuple[str, str] | None:
    if os.environ.get("ORPHEUS_DIARIZE_BACKEND", "").strip().lower() != "modal":
        return None
    url = os.environ.get("ORPHEUS_MODAL_DIARIZE_URL", "").strip()
    token = os.environ.get("ORPHEUS_MODAL_DIARIZE_TOKEN", "").strip()
    return (url, token) if url and token else None


def manifest_identity() -> tuple[str, str]:
    """(model_id, model_version_id) for the default diarizer.

    The catalog advertised ``pyannote`` unconditionally even though the default
    is the round-robin stub. Reflect the configured engine instead, so the
    manifest is honest and the content cache doesn't key a stub result under a
    pyannote version id.
    """
    if _modal_diarize_config():
        return "ecapa-diarize", ModalDiarizer.model_version_id
    model = os.environ.get("ORPHEUS_DIARIZE_MODEL")
    if model:
        return "pyannote", f"pyannote:{model}"
    return "stub-diarize", StubDiarizer.model_version_id


def get_diarizer(num_speakers: int = 2) -> Diarizer:
    """Return the configured diarizer: Modal GPU (ECAPA) if enabled, else pyannote
    when configured + importable, else the deterministic stub."""
    modal_cfg = _modal_diarize_config()
    if modal_cfg:
        return ModalDiarizer(modal_cfg[0], modal_cfg[1], max_speakers=num_speakers)
    model = os.environ.get("ORPHEUS_DIARIZE_MODEL")
    if model:
        try:
            token = os.environ.get("HF_TOKEN") or os.environ.get("HUGGING_FACE_HUB_TOKEN")
            return PyannoteDiarizer(model, token)
        except Exception:  # pragma: no cover - missing extra / model / token
            pass
    return StubDiarizer(num_speakers=num_speakers)
