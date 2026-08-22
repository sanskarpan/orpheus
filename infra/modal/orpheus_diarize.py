"""Orpheus speaker-diarization service on Modal GPU.

Real diarization without a gated model: SpeechBrain's **ECAPA-TDNN** speaker
embeddings (Apache-2.0, non-gated) over VAD-gated sliding windows, clustered
with agglomerative clustering. Replaces the worker's round-robin stub when
``ORPHEUS_DIARIZE_BACKEND=modal`` is set.

Endpoint: POST JSON ``{token, audio_b64 (16 kHz mono wav), num_speakers?,
max_speakers?}`` → ``{turns: [{start,end,speaker}], num_speakers,
model_version_id, gpu_seconds}``. Auth is the shared secret in
``orpheus-modal-auth`` (X-Orpheus-Token via ``token``).

Deploy: modal deploy infra/modal/orpheus_diarize.py
"""

from __future__ import annotations

import modal

APP_NAME = "orpheus-diarize"
CACHE_DIR = "/cache"
WINDOW_S = 1.5
HOP_S = 0.75

image = (
    modal.Image.debian_slim(python_version="3.12")
    .apt_install("ffmpeg")
    .pip_install(
        "speechbrain==1.0.2",
        "torch==2.5.1",
        "torchaudio==2.5.1",
        "scikit-learn==1.5.2",
        "soundfile==0.12.1",
        "numpy<2",
        "requests",
        "huggingface_hub==0.25.2",  # speechbrain 1.0.2 still passes use_auth_token
        "fastapi[standard]",
    )
    .env({"HF_HOME": CACHE_DIR, "SPEECHBRAIN_CACHE": CACHE_DIR})
)

app = modal.App(APP_NAME)
model_cache = modal.Volume.from_name("orpheus-diarize-cache", create_if_missing=True)
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
class Diarizer:
    @modal.enter()
    def load(self):
        import torch
        from speechbrain.inference.speaker import EncoderClassifier

        self.torch = torch
        self.device = "cuda" if torch.cuda.is_available() else "cpu"
        self.model = EncoderClassifier.from_hparams(
            source="speechbrain/spkrec-ecapa-voxceleb",
            savedir=f"{CACHE_DIR}/ecapa",
            run_opts={"device": self.device},
        )
        model_cache.commit()

    def _embed(self, windows):
        import numpy as np

        t = self.torch.tensor(np.stack(windows)).float().to(self.device)
        with self.torch.no_grad():
            emb = self.model.encode_batch(t).squeeze(1)  # [B, D]
        emb = self.torch.nn.functional.normalize(emb, dim=-1)
        return emb.cpu().numpy()

    @modal.method()
    def embed(self, payload: dict) -> dict:
        import base64
        import io
        import time

        import numpy as np
        import soundfile as sf

        t0 = time.monotonic()
        raw = base64.b64decode(payload.get("audio_b64") or "")
        if not raw:
            return {
                "embedding": [],
                "dim": 0,
                "model_version_id": "ecapa-agglomerative-1",
                "gpu_seconds": 0.0,
            }
        audio, sr = sf.read(io.BytesIO(raw), dtype="float32")
        if audio.ndim > 1:
            audio = audio.mean(axis=1)
        win = int(WINDOW_S * sr)
        hop = int(HOP_S * sr)
        global_rms = float(np.sqrt(np.mean(audio**2)) + 1e-9)
        windows = []
        for start in range(0, max(1, len(audio) - win + 1), hop):
            seg = audio[start : start + win]
            if seg.size and float(np.sqrt(np.mean(seg**2))) >= 0.35 * global_rms:
                windows.append(seg)
        if not windows:
            base = (
                audio[:win] if len(audio) >= win else np.pad(audio, (0, max(0, win - len(audio))))
            )
            windows = [base]
        emb = self._embed(windows)  # [N, D], per-window unit-norm
        mean = emb.mean(axis=0)
        norm = float(np.linalg.norm(mean))
        mean = (mean / norm) if norm else mean
        return {
            "embedding": [float(x) for x in mean],
            "dim": int(mean.shape[0]),
            "model_version_id": "ecapa-agglomerative-1",
            "gpu_seconds": round(time.monotonic() - t0, 4),
        }

    @modal.method()
    def diarize(self, payload: dict) -> dict:
        import base64
        import io
        import time

        import numpy as np
        import soundfile as sf
        from sklearn.cluster import AgglomerativeClustering
        from sklearn.metrics import silhouette_score

        t0 = time.monotonic()
        raw = base64.b64decode(payload["audio_b64"])
        audio, sr = sf.read(io.BytesIO(raw), dtype="float32")
        if audio.ndim > 1:
            audio = audio.mean(axis=1)
        num_speakers = int(payload.get("num_speakers") or 0)
        max_speakers = int(payload.get("max_speakers") or 6)

        win = int(WINDOW_S * sr)
        hop = int(HOP_S * sr)
        if len(audio) < win:
            return {
                "turns": [{"start": 0.0, "end": len(audio) / sr, "speaker": "S1"}],
                "num_speakers": 1,
                "model_version_id": "ecapa-agglomerative-1",
                "gpu_seconds": round(time.monotonic() - t0, 4),
            }

        # Energy VAD: keep windows whose RMS is well above the global floor.
        global_rms = float(np.sqrt(np.mean(audio**2)) + 1e-9)
        windows, times = [], []
        for start in range(0, len(audio) - win + 1, hop):
            seg = audio[start : start + win]
            if float(np.sqrt(np.mean(seg**2))) >= 0.35 * global_rms:
                windows.append(seg)
                times.append((start / sr, (start + win) / sr))
        if not windows:
            return {
                "turns": [{"start": 0.0, "end": len(audio) / sr, "speaker": "S1"}],
                "num_speakers": 1,
                "model_version_id": "ecapa-agglomerative-1",
                "gpu_seconds": round(time.monotonic() - t0, 4),
            }

        emb = self._embed(windows)

        # Choose k: explicit, else pick the k in [2..max] with best silhouette,
        # falling back to a single speaker when separation is weak.
        if num_speakers >= 1:
            k = min(num_speakers, len(emb))
        elif len(emb) < 2:
            k = 1
        else:
            best_k, best_s = 1, -1.0
            # silhouette_score requires 2 <= n_labels <= n_samples - 1, so the
            # candidate k must stay below len(emb): a k == n_samples run yields one
            # label per window and raises ValueError. Cap the search at len(emb)-1.
            for cand in range(2, min(max_speakers, len(emb) - 1) + 1):
                labels = AgglomerativeClustering(
                    n_clusters=cand, metric="cosine", linkage="average"
                ).fit_predict(emb)
                n_labels = len(set(labels))
                if n_labels < 2 or n_labels >= len(emb):
                    continue
                s = silhouette_score(emb, labels, metric="cosine")
                if s > best_s:
                    best_k, best_s = cand, s
            k = best_k if best_s >= 0.10 else 1

        if k <= 1:
            labels = [0] * len(emb)
        else:
            labels = (
                AgglomerativeClustering(n_clusters=k, metric="cosine", linkage="average")
                .fit_predict(emb)
                .tolist()
            )

        # Merge consecutive same-speaker windows into turns.
        turns = []
        for (start, end), lab in zip(times, labels, strict=False):
            spk = f"S{int(lab) + 1}"
            if turns and turns[-1]["speaker"] == spk and start <= turns[-1]["end"] + HOP_S:
                turns[-1]["end"] = end
            else:
                turns.append({"start": round(start, 2), "end": round(end, 2), "speaker": spk})
        speakers = sorted({t["speaker"] for t in turns})
        return {
            "turns": turns,
            "num_speakers": len(speakers),
            "model_version_id": "ecapa-agglomerative-1",
            "gpu_seconds": round(time.monotonic() - t0, 4),
        }


@app.function(image=image, secrets=[auth], timeout=900)
@modal.fastapi_endpoint(method="POST")
def diarize(payload: dict):
    import os

    from fastapi import HTTPException

    expected = os.environ.get("ORPHEUS_MODAL_SHARED_SECRET", "")
    if not expected or (payload.get("token", "") if isinstance(payload, dict) else "") != expected:
        raise HTTPException(status_code=401, detail="invalid or missing token")
    return Diarizer().diarize.remote(payload)


@app.function(image=image, secrets=[auth], timeout=900)
@modal.fastapi_endpoint(method="POST")
def embed(payload: dict):
    """Speaker-voiceprint embedding for enrollment/recognition (PRD 13 §4.8).

    POST ``{token, audio_b64 (16 kHz mono wav)}`` → ``{embedding (unit-norm),
    dim, model_version_id, gpu_seconds}``. Mean of VAD-gated ECAPA window
    embeddings — the same geometry the diarizer clusters on.
    """
    import os

    from fastapi import HTTPException

    expected = os.environ.get("ORPHEUS_MODAL_SHARED_SECRET", "")
    if not expected or (payload.get("token", "") if isinstance(payload, dict) else "") != expected:
        raise HTTPException(status_code=401, detail="invalid or missing token")
    return Diarizer().embed.remote(payload)
