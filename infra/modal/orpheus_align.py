"""Orpheus forced-alignment service on Modal GPU.

Tightens word-level timestamps against the acoustics using torchaudio's **MMS_FA**
forced-alignment bundle (multilingual wav2vec2 CTC, non-gated, CC-BY-NC-4.0 model
weights). Given the audio plus the known transcript per segment, it emits precise
per-word ``{start, end}`` boundaries — far tighter than Whisper's attention-derived
timings — for karaoke subtitles and frame-accurate clipping.

Endpoint: POST JSON ``{token, audio_b64 (16 kHz mono wav), language?,
segments: [{start, end, text}]}`` → ``{segments: [{words: [{word,start,end,
confidence}]}], model_version_id, gpu_seconds}``. Auth is the shared secret in
``orpheus-modal-auth`` (via ``token``). Selected by the worker when
``ORPHEUS_ALIGN_BACKEND=modal`` + ``ORPHEUS_MODAL_ALIGN_URL/TOKEN``.

Deploy: modal deploy infra/modal/orpheus_align.py
"""

from __future__ import annotations

import re

import modal

APP_NAME = "orpheus-align"
CACHE_DIR = "/cache"
MODEL_VERSION_ID = "torchaudio-mms_fa-1"
_WORD_RE = re.compile("[^\\W_]+(?:['\u2019][^\\W_]+)?", re.UNICODE)

image = (
    modal.Image.debian_slim(python_version="3.12")
    .apt_install("ffmpeg")
    .pip_install(
        "torch==2.5.1",
        "torchaudio==2.5.1",
        "soundfile==0.12.1",
        "numpy<2",
        "fastapi[standard]",
    )
    .env({"TORCH_HOME": CACHE_DIR})
)

app = modal.App(APP_NAME)
model_cache = modal.Volume.from_name("orpheus-align-cache", create_if_missing=True)
auth = modal.Secret.from_name("orpheus-modal-auth")


def _tokens(text: str) -> list[str]:
    return _WORD_RE.findall(text or "")


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
class Aligner:
    @modal.enter()
    def load(self):
        import torch
        from torchaudio.pipelines import MMS_FA as bundle  # noqa: N811 (bundle is an object)

        self.torch = torch
        self.bundle = bundle
        self.device = "cuda" if torch.cuda.is_available() else "cpu"
        self.model = bundle.get_model().to(self.device)
        self.tokenizer = bundle.get_tokenizer()
        self.aligner = bundle.get_aligner()
        self.sample_rate = bundle.sample_rate
        model_cache.commit()

    def _align_segment(self, waveform, sr: int, seg: dict) -> dict:
        tokens = _tokens(seg.get("text", ""))
        start = float(seg.get("start", 0.0))
        end = float(seg.get("end", start))
        a, b = int(start * sr), int(end * sr)
        clip = waveform[:, a:b]
        if not tokens or clip.shape[1] < sr // 100:  # <10ms or empty
            return {"words": []}
        with self.torch.inference_mode():
            emission, _ = self.model(clip.to(self.device))
            spans = self.aligner(emission[0], self.tokenizer([t.lower() for t in tokens]))
        ratio = clip.shape[1] / emission.shape[1] / sr
        words = []
        for tok, sp in zip(tokens, spans, strict=False):
            length = sum(s.end - s.start for s in sp) or 1
            score = sum(s.score * (s.end - s.start) for s in sp) / length
            words.append(
                {
                    "word": tok,
                    "start": round(start + sp[0].start * ratio, 3),
                    "end": round(start + sp[-1].end * ratio, 3),
                    "confidence": round(float(score), 3),
                }
            )
        return {"words": words}

    @modal.method()
    def align(self, payload: dict) -> dict:
        import base64
        import io
        import time

        import soundfile as sf

        t0 = time.monotonic()
        segments = payload.get("segments") or []
        raw = base64.b64decode(payload.get("audio_b64") or "")
        if not raw or not segments:  # nothing to align → clean empty result, not a 500
            return {
                "segments": [{"words": []} for _ in segments],
                "model_version_id": MODEL_VERSION_ID,
                "gpu_seconds": round(time.monotonic() - t0, 4),
            }
        audio, sr = sf.read(io.BytesIO(raw), dtype="float32")
        waveform = self.torch.from_numpy(audio).float()
        if waveform.ndim > 1:
            waveform = waveform.mean(dim=1)
        waveform = waveform.unsqueeze(0)
        if sr != self.sample_rate:
            import torchaudio

            waveform = torchaudio.functional.resample(waveform, sr, self.sample_rate)
            sr = self.sample_rate

        out = []
        for seg in segments:
            try:
                out.append(self._align_segment(waveform, sr, seg))
            except Exception:  # OOV/numeric token etc. — degrade this segment only
                out.append({"words": []})
        return {
            "segments": out,
            "model_version_id": MODEL_VERSION_ID,
            "gpu_seconds": round(time.monotonic() - t0, 4),
        }


@app.function(image=image, secrets=[auth], timeout=900)
@modal.fastapi_endpoint(method="POST")
def align(payload: dict):
    import os

    from fastapi import HTTPException

    expected = os.environ.get("ORPHEUS_MODAL_SHARED_SECRET", "")
    if not expected or (payload.get("token", "") if isinstance(payload, dict) else "") != expected:
        raise HTTPException(status_code=401, detail="invalid or missing token")
    return Aligner().align.remote(payload)
