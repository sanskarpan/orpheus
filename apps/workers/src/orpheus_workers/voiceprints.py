"""Speaker voiceprint embedding + matching (PRD 03 §4.8).

``get_embedder().embed(wav_path)`` returns a unit-norm speaker embedding.
``ModalEmbedder`` offloads to the Modal diarize service's ``/embed`` endpoint
(ECAPA) when ``ORPHEUS_MODAL_EMBED_URL`` + ``ORPHEUS_MODAL_DIARIZE_TOKEN`` are set;
otherwise ``StubEmbedder`` produces a deterministic vector from the audio bytes so
enrollment/recognition logic is testable without GPU.

``match_profile(embedding, profiles, threshold)`` cosine-matches an embedding
against enrolled ``speaker_profiles`` rows and returns the best match (or None).
Biometric data: enrollment is explicit + consent-gated at the processor layer.
"""

from __future__ import annotations

import base64
import hashlib
import math
import os
from pathlib import Path
from typing import Any, Protocol

_EMBED_MODEL_VERSION = "ecapa-agglomerative-1"


class Embedder(Protocol):
    model_version_id: str

    def embed(self, wav_path: str | Path) -> list[float]: ...


class StubEmbedder:
    """Deterministic, dependency-free embedding from the audio bytes (tests).

    Not a real voiceprint — a hash-seeded unit vector — but identical audio maps
    to an identical embedding, so enroll→identify round-trips are testable.
    """

    model_version_id = "stub-embed-1"
    dim = 32

    def embed(self, wav_path: str | Path) -> list[float]:
        # Hash the PCM samples (not the container header) so the same audio embeds
        # identically whether it's the original file or a re-written clip.
        import contextlib
        import wave

        raw = b""
        with contextlib.suppress(Exception), wave.open(str(wav_path), "rb") as w:
            raw = w.readframes(w.getnframes())
        if not raw:
            raw = Path(wav_path).read_bytes()
        vec = []
        seed = hashlib.sha256(raw).digest()
        for i in range(self.dim):
            b = seed[i % len(seed)]
            vec.append((b / 255.0) * 2.0 - 1.0)
        return _normalize(vec)


class ModalEmbedder:
    """Real ECAPA embedding via the Modal diarize ``/embed`` endpoint."""

    model_version_id = _EMBED_MODEL_VERSION

    def __init__(self, url: str, token: str) -> None:
        self._url = url
        self._token = token

    def embed(self, wav_path: str | Path) -> list[float]:
        from .modal_client import modal_post_json

        payload = {
            "token": self._token,
            "audio_b64": base64.b64encode(Path(wav_path).read_bytes()).decode(),
        }
        data = modal_post_json(self._url, payload).json()
        self.model_version_id = data.get("model_version_id", self.model_version_id)
        return _normalize([float(x) for x in data.get("embedding", [])])


def _modal_embed_config() -> tuple[str, str] | None:
    url = os.environ.get("ORPHEUS_MODAL_EMBED_URL", "").strip()
    token = os.environ.get("ORPHEUS_MODAL_DIARIZE_TOKEN", "").strip()
    if url and token and os.environ.get("ORPHEUS_DIARIZE_BACKEND", "").lower() == "modal":
        return url, token
    return None


def get_embedder() -> Embedder:
    cfg = _modal_embed_config()
    if cfg:
        return ModalEmbedder(cfg[0], cfg[1])
    return StubEmbedder()


def _normalize(vec: list[float]) -> list[float]:
    n = math.sqrt(sum(x * x for x in vec))
    if n == 0:
        return vec
    return [x / n for x in vec]


def cosine(a: list[float], b: list[float]) -> float:
    if not a or not b or len(a) != len(b):
        return -1.0
    return sum(x * y for x, y in zip(a, b, strict=False))  # inputs are unit-norm


def match_profile(
    embedding: list[float], profiles: list[dict[str, Any]], threshold: float
) -> dict[str, Any] | None:
    """Return the best-matching enrolled profile at/above ``threshold``, else None.

    ``profiles`` are rows with at least ``name`` and ``embedding`` (unit-norm).
    Profiles whose embedding dimension differs (a different model) are skipped so
    voiceprints never match across incompatible embedding spaces.
    """
    best, best_sim = None, threshold
    for p in profiles:
        emb = p.get("embedding") or []
        if len(emb) != len(embedding):
            continue
        sim = cosine(embedding, list(emb))
        if sim >= best_sim:
            best, best_sim = p, sim
    if best is None:
        return None
    return {"name": best.get("name"), "id": best.get("id"), "score": round(best_sim, 4)}
