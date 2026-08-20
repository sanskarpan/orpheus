"""Orpheus audio-enhancement service on Modal GPU (PRD 14).

Neural, Krisp-class enhancement with open, non-gated models:
- ``denoise`` / ``dereverb`` / ``telephony`` — **SpeechBrain MetricGAN+** speech
  enhancement (Apache-2.0, the same non-gated SpeechBrain family as diarization).
- ``isolate`` / ``background_voice`` — **Demucs** (htdemucs) vocals stem (MIT).
- ``accent`` — not a neural model here; returns the enhanced signal + a warning
  (documented limitation; accent conversion is a separate roadmap item).

Endpoint: POST JSON ``{token, audio_b64 (wav), mode}`` → ``{audio_b64 (cleaned
16 kHz mono wav), metrics, warnings, model_version_id, gpu_seconds}``. Auth is the
shared secret in ``orpheus-modal-auth``. Selected by the worker when
``ORPHEUS_ENHANCE_BACKEND=modal`` + ``ORPHEUS_MODAL_ENHANCE_URL``/``_TOKEN``.

Deploy: modal deploy infra/modal/orpheus_enhance.py
"""

from __future__ import annotations

import modal

APP_NAME = "orpheus-enhance"
CACHE_DIR = "/cache"
MODEL_VERSION_ID = "metricgan-plus+demucs-1"

image = (
    modal.Image.debian_slim(python_version="3.12")
    .apt_install("ffmpeg")
    .pip_install(
        "speechbrain==1.0.2",
        "demucs==4.0.1",
        "torch==2.5.1",
        "torchaudio==2.5.1",
        "soundfile==0.12.1",
        "numpy<2",
        "huggingface_hub==0.25.2",
        "fastapi[standard]",
    )
    .env({"HF_HOME": CACHE_DIR, "SPEECHBRAIN_CACHE": CACHE_DIR, "TORCH_HOME": CACHE_DIR})
)

app = modal.App(APP_NAME)
model_cache = modal.Volume.from_name("orpheus-enhance-cache", create_if_missing=True)
auth = modal.Secret.from_name("orpheus-modal-auth")

_DF_MODES = {"denoise", "dereverb", "telephony", "accent"}
_DEMUCS_MODES = {"isolate", "background_voice"}


@app.cls(
    image=image,
    gpu="a10g",
    volumes={CACHE_DIR: model_cache},
    secrets=[auth],
    min_containers=0,
    scaledown_window=300,
    timeout=900,
)
@modal.concurrent(max_inputs=2)
class Enhancer:
    @modal.enter()
    def load(self):
        import torch
        from speechbrain.inference.enhancement import SpectralMaskEnhancement

        self.torch = torch
        self.device = "cuda" if torch.cuda.is_available() else "cpu"
        self.enhancer = SpectralMaskEnhancement.from_hparams(
            source="speechbrain/metricgan-plus-voicebank",
            savedir=f"{CACHE_DIR}/metricgan",
            run_opts={"device": self.device},
        )
        self._demucs = None  # lazy — only voice-isolation modes need it
        model_cache.commit()

    def _demucs_model(self):
        if self._demucs is None:
            from demucs.pretrained import get_model

            self._demucs = get_model("htdemucs").to(self.device)
            self._demucs.eval()
        return self._demucs

    def _rms(self, x):
        import numpy as np

        return float(np.sqrt(np.mean(x.astype(np.float64) ** 2)) + 1e-9) if len(x) else 0.0

    def _metricgan(self, audio_16k):
        """MetricGAN+ speech enhancement on a mono 16 kHz numpy signal → numpy."""
        noisy = self.torch.from_numpy(audio_16k).float().unsqueeze(0).to(self.device)
        lengths = self.torch.tensor([1.0]).to(self.device)
        with self.torch.no_grad():
            enhanced = self.enhancer.enhance_batch(noisy, lengths=lengths)
        return enhanced.squeeze(0).cpu().numpy()

    def _isolate(self, wav_tensor, sr):
        import torchaudio
        from demucs.apply import apply_model

        model = self._demucs_model()
        wav = wav_tensor
        if sr != model.samplerate:
            wav = torchaudio.functional.resample(wav, sr, model.samplerate)
        if wav.shape[0] == 1:
            wav = wav.repeat(2, 1)  # demucs expects stereo
        ref = wav.mean(0)
        wav = (wav - ref.mean()) / (ref.std() + 1e-8)
        with self.torch.no_grad():
            stems = apply_model(model, wav[None].to(self.device), device=self.device)[0]
        vocals = stems[model.sources.index("vocals")].mean(0).cpu()  # mono vocals
        return vocals, model.samplerate

    @modal.method()
    def enhance(self, payload: dict) -> dict:
        import base64
        import io
        import time

        import numpy as np
        import soundfile as sf
        import torch
        import torchaudio

        t0 = time.monotonic()
        raw = base64.b64decode(payload.get("audio_b64") or "")
        mode = str(payload.get("mode", "denoise")).lower()
        warnings = []
        if not raw:
            return {"audio_b64": "", "metrics": {}, "warnings": ["empty_audio"],
                    "model_version_id": MODEL_VERSION_ID, "gpu_seconds": 0.0}

        audio, sr = sf.read(io.BytesIO(raw), dtype="float32")
        if audio.ndim > 1:
            audio = audio.mean(axis=1)
        in_rms = self._rms(audio)

        if mode in _DEMUCS_MODES:
            wav_t = torch.from_numpy(audio).float().unsqueeze(0)
            out, out_sr = self._isolate(wav_t, sr)
            cleaned = out.numpy()
            cur_sr = out_sr
        else:
            if mode == "accent":
                warnings.append("accent_not_supported_enhanced_instead")
            # MetricGAN+ operates at 16 kHz.
            wav_t = torch.from_numpy(audio).float().unsqueeze(0)
            if sr != 16000:
                wav_t = torchaudio.functional.resample(wav_t, sr, 16000)
            cleaned = self._metricgan(wav_t.squeeze(0).numpy())
            cur_sr = 16000

        # normalize output to 16 kHz mono wav
        out_t = torch.from_numpy(np.ascontiguousarray(cleaned)).float().unsqueeze(0)
        if cur_sr != 16000:
            out_t = torchaudio.functional.resample(out_t, cur_sr, 16000)
        out_np = out_t.squeeze(0).numpy()
        buf = io.BytesIO()
        sf.write(buf, out_np, 16000, format="WAV", subtype="PCM_16")
        out_rms = self._rms(out_np)
        reduction_db = 20.0 * np.log10(in_rms / (out_rms + 1e-9)) if out_rms > 0 else 0.0
        return {
            "audio_b64": base64.b64encode(buf.getvalue()).decode(),
            "metrics": {
                "input_rms": round(in_rms, 2),
                "output_rms": round(out_rms, 2),
                "reduction_db": round(float(reduction_db), 2),
                "sample_rate": 16000,
                "mode": mode,
            },
            "warnings": warnings,
            "model_version_id": MODEL_VERSION_ID,
            "gpu_seconds": round(time.monotonic() - t0, 4),
        }


@app.function(image=image, secrets=[auth], timeout=900)
@modal.fastapi_endpoint(method="POST")
def enhance(payload: dict):
    import os

    from fastapi import HTTPException

    expected = os.environ.get("ORPHEUS_MODAL_SHARED_SECRET", "")
    if not expected or (payload.get("token", "") if isinstance(payload, dict) else "") != expected:
        raise HTTPException(status_code=401, detail="invalid or missing token")
    return Enhancer().enhance.remote(payload)
