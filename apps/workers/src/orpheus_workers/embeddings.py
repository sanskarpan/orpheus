"""Text-embedding provider + ranking for the transcript index (PRD 06 #359/#360).

``get_text_embedder().embed(texts)`` returns unit-norm sentence embeddings.
``ModalTextEmbedder`` offloads to the Modal all-MiniLM service when
``ORPHEUS_MODAL_EMBED_TEXT_URL`` + ``ORPHEUS_MODAL_EMBED_TEXT_TOKEN`` are set;
otherwise ``StubTextEmbedder`` produces deterministic vectors so the index /
search / ask logic is testable without a model.

``rank_by_similarity`` cosine-ranks stored embeddings against a query embedding
(inputs are unit-norm, so cosine = dot product).
"""

from __future__ import annotations

import hashlib
import math
import os
import struct
from typing import Any, Protocol


class TextEmbedder(Protocol):
    model_version_id: str

    def embed(self, texts: list[str]) -> list[list[float]]: ...


class StubTextEmbedder:
    """Deterministic, dependency-free embeddings (tests / model-less deploys).

    A hash-seeded unit vector per text — identical text → identical vector, and
    the same text is maximally similar to itself, so retrieval logic is testable.
    """

    model_version_id = "stub-embed-text-1"
    dim = 16

    def embed(self, texts: list[str]) -> list[list[float]]:
        out = []
        for t in texts:
            digest = hashlib.sha256((t or "").strip().lower().encode()).digest()
            vec = [struct.unpack("<h", digest[i : i + 2])[0] / 32768.0 for i in range(0, self.dim * 2, 2)]
            out.append(_normalize(vec))
        return out


class ModalTextEmbedder:
    """Real sentence embeddings via the Modal all-MiniLM endpoint."""

    model_version_id = "all-minilm-l6-v2-1"

    def __init__(self, url: str, token: str) -> None:
        self._url = url
        self._token = token

    def embed(self, texts: list[str]) -> list[list[float]]:
        if not texts:
            return []
        from .modal_client import modal_post_json

        data = modal_post_json(self._url, {"token": self._token, "texts": texts}).json()
        self.model_version_id = data.get("model_version_id", self.model_version_id)
        return [_normalize([float(x) for x in row]) for row in data.get("embeddings", [])]


def _modal_embed_text_config() -> tuple[str, str] | None:
    url = os.environ.get("ORPHEUS_MODAL_EMBED_TEXT_URL", "").strip()
    token = os.environ.get("ORPHEUS_MODAL_EMBED_TEXT_TOKEN", "").strip()
    return (url, token) if url and token else None


def get_text_embedder() -> TextEmbedder:
    cfg = _modal_embed_text_config()
    if cfg:
        return ModalTextEmbedder(cfg[0], cfg[1])
    return StubTextEmbedder()


def manifest_identity() -> tuple[str, str]:
    if _modal_embed_text_config():
        return "orpheus-embed", ModalTextEmbedder.model_version_id
    return "stub-embed-text", StubTextEmbedder.model_version_id


def _normalize(vec: list[float]) -> list[float]:
    n = math.sqrt(sum(x * x for x in vec))
    return [x / n for x in vec] if n else vec


def cosine(a: list[float], b: list[float]) -> float:
    if not a or not b or len(a) != len(b):
        return -1.0
    return sum(x * y for x, y in zip(a, b, strict=False))  # unit-norm inputs


def rank_by_similarity(
    query: list[float], rows: list[dict[str, Any]], top_k: int
) -> list[dict[str, Any]]:
    """Return the ``top_k`` rows most similar to ``query`` (each gets a ``score``).

    ``rows`` carry an ``embedding`` (unit-norm). Rows whose dimension differs from
    the query are skipped (never match across incompatible embedding spaces).
    """
    scored = []
    for r in rows:
        emb = r.get("embedding") or []
        if len(emb) != len(query):
            continue
        scored.append({**r, "score": round(cosine(query, list(emb)), 4)})
    scored.sort(key=lambda r: r["score"], reverse=True)
    return scored[:top_k]
