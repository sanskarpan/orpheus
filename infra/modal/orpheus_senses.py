"""Orpheus speech-emotion + audio-event service on Modal GPU (PRD 13 §4.1-4.3).

Runs **SenseVoice-Small** (FunASR, open, non-gated) which emits a spoken-emotion
tag (SER) and audio-event tags (AED) in a single pass. Given the audio plus
optional transcript segments, it returns per-segment emotion + events.

Endpoint: POST JSON ``{token, audio_b64 (16 kHz mono wav), segments?:[{start,end}]}``
→ ``{segments:[{start,end,emotion,emotion_scores,events:[{label,confidence}]}],
model_version_id, gpu_seconds}``. Auth is the shared secret in
``orpheus-modal-auth`` (via ``token``). Selected by the worker when
``ORPHEUS_SENSE_BACKEND=modal`` + ``ORPHEUS_MODAL_SENSE_URL``/``ORPHEUS_MODAL_SENSE_TOKEN``.

Deploy: modal deploy infra/modal/orpheus_senses.py
"""

from __future__ import annotations

import re

import modal

APP_NAME = "orpheus-senses"
CACHE_DIR = "/cache"
MODEL_VERSION_ID = "sensevoice-small-1"
_TOKEN_RE = re.compile(r"<\|([^|>]+)\|>")

# SenseVoice emotion tags → our lowercase label space.
_EMOTION_MAP = {
    "HAPPY": "happy",
    "SAD": "sad",
    "ANGRY": "angry",
    "NEUTRAL": "neutral",
    "FEARFUL": "fearful",
    "DISGUSTED": "disgusted",
    "SURPRISED": "surprised",
    "EMO_UNKNOWN": "neutral",
}
# SenseVoice audio-event tags → friendly labels (others pass through verbatim).
_EVENT_MAP = {
    "BGM": "Music",
    "Speech": "Speech",
    "Applause": "Applause",
    "Laughter": "Laughter",
    "Cry": "Crying",
    "Cough": "Cough",
    "Sneeze": "Sneeze",
    "Breath": "Breath",
}
_NON_EVENTS = set(_EMOTION_MAP) | {"zh", "en", "yue", "ja", "ko", "nospeech", "withitn", "woitn"}

image = (
    modal.Image.debian_slim(python_version="3.12")
    .apt_install("ffmpeg")
    .pip_install(
        "funasr==1.1.12",
        "torch==2.5.1",
        "torchaudio==2.5.1",
        "soundfile==0.12.1",
        "numpy<2",
        "modelscope",
        "fastapi[standard]",
    )
    .env({"MODELSCOPE_CACHE": CACHE_DIR, "HF_HOME": CACHE_DIR})
)

app = modal.App(APP_NAME)
model_cache = modal.Volume.from_name("orpheus-senses-cache", create_if_missing=True)
auth = modal.Secret.from_name("orpheus-modal-auth")


def _parse_tags(raw: str) -> tuple[str, list[str]]:
    """Pull the emotion + event tags out of SenseVoice's rich token string."""
    tags = _TOKEN_RE.findall(raw)
    emotion = "neutral"
    events: list[str] = []
    for t in tags:
        if t in _EMOTION_MAP:
            emotion = _EMOTION_MAP[t]
        elif t not in _NON_EVENTS:
            events.append(_EVENT_MAP.get(t, t))
    return emotion, events


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
class Senses:
    @modal.enter()
    def load(self):
        import torch
        from funasr import AutoModel

        self.torch = torch
        self.device = "cuda" if torch.cuda.is_available() else "cpu"
        # SenseVoiceSmall ships custom model code → trust_remote_code is required.
        self.model = AutoModel(
            model="iic/SenseVoiceSmall",
            trust_remote_code=True,
            device=self.device,
            disable_update=True,
        )
        model_cache.commit()

    def _analyze_clip(self, audio, sr: int) -> dict:
        res = self.model.generate(
            input=audio, cache={}, language="auto", use_itn=True, ban_emo_unk=False
        )
        raw = res[0]["text"] if res else ""
        emotion, events = _parse_tags(raw)
        return {
            "emotion": emotion,
            "emotion_scores": {emotion: 1.0},
            "events": [{"label": e, "confidence": 1.0} for e in events]
            or [{"label": "Speech", "confidence": 1.0}],
        }

    @modal.method()
    def analyze(self, payload: dict) -> dict:
        import base64
        import io
        import time

        import soundfile as sf

        t0 = time.monotonic()
        raw = base64.b64decode(payload.get("audio_b64") or "")
        segments = payload.get("segments") or []
        if not raw:
            return {"segments": [], "model_version_id": MODEL_VERSION_ID, "gpu_seconds": 0.0}
        audio, sr = sf.read(io.BytesIO(raw), dtype="float32")
        if audio.ndim > 1:
            audio = audio.mean(axis=1)

        # No segments → analyze the whole file as one span.
        spans = segments or [{"start": 0.0, "end": len(audio) / sr}]
        out = []
        for seg in spans:
            a = max(0, int(float(seg.get("start", 0.0)) * sr))
            b = min(len(audio), int(float(seg.get("end", len(audio) / sr)) * sr))
            clip = audio[a:b] if b > a else audio[a : a + sr]
            try:
                res = self._analyze_clip(clip, sr)
            except Exception:  # one segment failing must not sink the batch
                res = {
                    "emotion": "neutral",
                    "emotion_scores": {"neutral": 1.0},
                    "events": [{"label": "Speech", "confidence": 1.0}],
                }
            res["start"] = float(seg.get("start", 0.0))
            res["end"] = float(seg.get("end", len(audio) / sr))
            out.append(res)
        return {
            "segments": out,
            "model_version_id": MODEL_VERSION_ID,
            "gpu_seconds": round(time.monotonic() - t0, 4),
        }


@app.function(image=image, secrets=[auth], timeout=900)
@modal.fastapi_endpoint(method="POST")
def analyze(payload: dict):
    import os

    from fastapi import HTTPException

    expected = os.environ.get("ORPHEUS_MODAL_SHARED_SECRET", "")
    if not expected or (payload.get("token", "") if isinstance(payload, dict) else "") != expected:
        raise HTTPException(status_code=401, detail="invalid or missing token")
    return Senses().analyze.remote(payload)
