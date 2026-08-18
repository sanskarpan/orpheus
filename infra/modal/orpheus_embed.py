"""Orpheus text-embedding service on Modal GPU (PRD 06 #359/#360).

Sentence embeddings for the transcript index (semantic search + ask-AI). Runs
**sentence-transformers all-MiniLM-L6-v2** (MIT, non-gated, 384-d), the same
"open, non-gated model" philosophy as the rest of the platform. Returns unit-norm
embeddings so cosine similarity = dot product.

Endpoint: POST JSON ``{token, texts:[...]}`` → ``{embeddings:[[...]], dim,
model_version_id, gpu_seconds}``. Auth is the shared secret in
``orpheus-modal-auth``. Selected by the worker via
``ORPHEUS_MODAL_EMBED_TEXT_URL`` + ``ORPHEUS_MODAL_EMBED_TEXT_TOKEN``.

Deploy: modal deploy infra/modal/orpheus_embed.py
"""

from __future__ import annotations

import modal

APP_NAME = "orpheus-embed"
CACHE_DIR = "/cache"
MODEL = "sentence-transformers/all-MiniLM-L6-v2"
MODEL_VERSION_ID = "all-minilm-l6-v2-1"

image = (
    modal.Image.debian_slim(python_version="3.12")
    .pip_install(
        "sentence-transformers==3.3.1",
        "torch==2.5.1",
        "numpy<2",
        "fastapi[standard]",
    )
    .env({"HF_HOME": CACHE_DIR, "SENTENCE_TRANSFORMERS_HOME": CACHE_DIR})
)

app = modal.App(APP_NAME)
model_cache = modal.Volume.from_name("orpheus-embed-cache", create_if_missing=True)
auth = modal.Secret.from_name("orpheus-modal-auth")

_MAX_TEXTS = 512  # cap per request


@app.cls(
    image=image,
    gpu="a10g",
    volumes={CACHE_DIR: model_cache},
    secrets=[auth],
    min_containers=0,
    scaledown_window=300,
    timeout=900,
)
@modal.concurrent(max_inputs=8)
class Embedder:
    @modal.enter()
    def load(self):
        import torch
        from sentence_transformers import SentenceTransformer

        device = "cuda" if torch.cuda.is_available() else "cpu"
        self.model = SentenceTransformer(MODEL, device=device, cache_folder=CACHE_DIR)
        model_cache.commit()

    @modal.method()
    def embed(self, payload: dict) -> dict:
        import time

        t0 = time.monotonic()
        texts = payload.get("texts") or []
        if not isinstance(texts, list):
            texts = []
        texts = [str(t) for t in texts[:_MAX_TEXTS]]
        if not texts:
            return {"embeddings": [], "dim": 0, "model_version_id": MODEL_VERSION_ID,
                    "gpu_seconds": 0.0}
        # normalize_embeddings=True → unit-norm, so cosine == dot product downstream.
        embs = self.model.encode(
            texts, batch_size=64, normalize_embeddings=True, convert_to_numpy=True
        )
        return {
            "embeddings": [[float(x) for x in row] for row in embs],
            "dim": int(embs.shape[1]) if len(embs) else 0,
            "model_version_id": MODEL_VERSION_ID,
            "gpu_seconds": round(time.monotonic() - t0, 4),
        }


@app.function(image=image, secrets=[auth], timeout=900)
@modal.fastapi_endpoint(method="POST")
def embed(payload: dict):
    import os

    from fastapi import HTTPException

    expected = os.environ.get("ORPHEUS_MODAL_SHARED_SECRET", "")
    if not expected or (payload.get("token", "") if isinstance(payload, dict) else "") != expected:
        raise HTTPException(status_code=401, detail="invalid or missing token")
    return Embedder().embed.remote(payload)
