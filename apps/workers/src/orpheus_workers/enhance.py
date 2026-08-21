"""Audio enhancement engine (PRD 14) — noise suppression + related front-ends.

``get_enhancer().enhance(wav_in, wav_out, mode)`` cleans a 16-bit PCM wav and
returns quality metrics. ``ModalEnhancer`` offloads the neural modes to the Modal
GPU service (DeepFilterNet / Demucs, ``orpheus_enhance.py``) when
``ORPHEUS_ENHANCE_BACKEND=modal`` + ``ORPHEUS_MODAL_ENHANCE_URL``/``_TOKEN`` are
set; otherwise ``LocalEnhancer`` runs a dependency-free numpy **spectral-gate**
denoiser in-worker (cheap, low-latency) — the same fall-back philosophy as
diarization's stub.

Modes: ``denoise`` (default), ``dereverb``, ``isolate`` (voice isolation),
``background_voice``, ``telephony`` (8 kHz band denoise), ``accent``. The local
enhancer implements the spectral denoise family; GPU-only modes degrade to the
spectral denoiser locally (with a ``warnings`` note) rather than failing.
"""

from __future__ import annotations

import base64
import math
import os
import wave
from pathlib import Path
from typing import Any, Protocol

# Modes the local numpy path can honor directly; others need the Modal backend.
_LOCAL_MODES = {"denoise", "telephony", "dereverb"}
_GPU_ONLY_MODES = {"isolate", "background_voice", "accent"}


class Enhancer(Protocol):
    model_version_id: str

    def enhance(self, wav_in: str | Path, wav_out: str | Path, mode: str) -> dict: ...


def _read_wav(path: str | Path) -> tuple[Any, int]:
    import numpy as np

    with wave.open(str(path), "rb") as w:
        sr = w.getframerate()
        ch = w.getnchannels()
        x = np.frombuffer(w.readframes(w.getnframes()), dtype=np.int16).astype(np.float32)
    if ch > 1:
        x = x.reshape(-1, ch).mean(axis=1)
    return x, sr


def _write_wav(path: str | Path, x: Any, sr: int) -> None:
    import numpy as np

    clipped = np.clip(x, -32768, 32767).astype(np.int16)
    with wave.open(str(path), "wb") as w:
        w.setnchannels(1)
        w.setsampwidth(2)
        w.setframerate(sr)
        w.writeframes(clipped.tobytes())


def _rms(x: Any) -> float:
    import numpy as np

    return float(np.sqrt(np.mean(x.astype(np.float64) ** 2)) + 1e-9) if len(x) else 0.0


def spectral_gate_denoise(x: Any, sr: int, strength: float = 1.5) -> Any:
    """Noise-suppress a mono float32 signal via STFT spectral subtraction.

    Estimates a noise magnitude profile from the quietest ~10% of frames (the
    non-speech / noise-only frames), over-subtracts it from every frame, and
    floors the result at a small fraction of the original magnitude (a spectral
    floor that curbs musical noise). ``strength`` scales the over-subtraction.
    Pure numpy — no model, no extra dependency.
    """
    import numpy as np

    if len(x) < 512:
        return x
    n_fft = 512
    hop = n_fft // 4
    window = np.hanning(n_fft).astype(np.float32)

    n_frames = 1 + (len(x) - n_fft) // hop
    if n_frames < 8:
        return x
    frames = np.stack([x[i * hop : i * hop + n_fft] * window for i in range(n_frames)])
    spec = np.fft.rfft(frames, axis=1)
    mag = np.abs(spec)
    phase = np.angle(spec)

    # noise profile = mean magnitude of the quietest frames (noise-only frames)
    frame_energy = mag.sum(axis=1)
    k = max(1, int(0.1 * n_frames))
    noise_frames = np.argsort(frame_energy)[:k]
    noise_mag = mag[noise_frames].mean(axis=0)

    over_sub = 1.5 + strength  # strength 1.5 → 3.0×; telephony 2.0 → 3.5×
    beta = 0.02  # spectral floor: keep 2% of the original magnitude
    clean_mag = np.maximum(mag - over_sub * noise_mag, beta * mag)
    clean_spec = clean_mag * np.exp(1j * phase)

    clean_frames = np.fft.irfft(clean_spec, n=n_fft, axis=1).astype(np.float32) * window
    out = np.zeros(len(x), dtype=np.float32)
    norm = np.zeros(len(x), dtype=np.float32)
    win_sq = window**2
    for i in range(n_frames):
        out[i * hop : i * hop + n_fft] += clean_frames[i]
        norm[i * hop : i * hop + n_fft] += win_sq
    nz = norm > 1e-6
    out[nz] /= norm[nz]
    return out


class LocalEnhancer:
    """Dependency-free numpy spectral-gate denoiser (cheap in-worker path)."""

    model_version_id = "spectral-gate-1"

    def enhance(self, wav_in: str | Path, wav_out: str | Path, mode: str) -> dict:
        import numpy as np

        x, sr = _read_wav(wav_in)
        in_rms = _rms(x)
        warnings: list[str] = []
        strength = 2.0 if mode == "telephony" else 1.5
        cleaned = spectral_gate_denoise(x, sr, strength=strength)
        if mode in _GPU_ONLY_MODES:
            # No local model for these — we still denoise so output beats input,
            # and flag that the neural mode was unavailable.
            warnings.append(f"mode_{mode}_requires_modal_backend_denoised_instead")
        out_rms = _rms(cleaned)
        # residual-noise estimate: energy removed relative to input
        removed = np.clip(x[: len(cleaned)] - cleaned, -32768, 32767)
        reduction_db = 20.0 * math.log10(in_rms / (out_rms + 1e-9)) if out_rms > 0 else 0.0
        _write_wav(wav_out, cleaned, sr)
        metrics = {
            "input_rms": round(in_rms, 2),
            "output_rms": round(out_rms, 2),
            "noise_removed_rms": round(_rms(removed), 2),
            "reduction_db": round(reduction_db, 2),
            "sample_rate": sr,
            "mode": mode,
        }
        return {"metrics": metrics, "warnings": warnings}


class ModalEnhancer:
    """Neural enhancement via the Modal GPU service (DeepFilterNet / Demucs)."""

    model_version_id = "modal-enhance-1"

    def __init__(self, url: str, token: str) -> None:
        self._url = url
        self._token = token

    def enhance(self, wav_in: str | Path, wav_out: str | Path, mode: str) -> dict:
        from .modal_client import modal_post_json

        payload = {
            "token": self._token,
            "audio_b64": base64.b64encode(Path(wav_in).read_bytes()).decode(),
            "mode": mode,
        }
        data = modal_post_json(self._url, payload).json()
        audio_b64 = data.get("audio_b64")
        if not audio_b64:
            raise RuntimeError("enhance service returned no audio")
        Path(wav_out).write_bytes(base64.b64decode(audio_b64))
        self.model_version_id = data.get("model_version_id", self.model_version_id)
        return {"metrics": data.get("metrics", {}), "warnings": data.get("warnings", [])}


def _modal_enhance_config() -> tuple[str, str] | None:
    if os.environ.get("ORPHEUS_ENHANCE_BACKEND", "").strip().lower() != "modal":
        return None
    url = os.environ.get("ORPHEUS_MODAL_ENHANCE_URL", "").strip()
    token = os.environ.get("ORPHEUS_MODAL_ENHANCE_TOKEN", "").strip()
    return (url, token) if url and token else None


def manifest_identity() -> tuple[str, str]:
    if _modal_enhance_config():
        return "orpheus-enhance", ModalEnhancer.model_version_id
    return "spectral-gate", LocalEnhancer.model_version_id


def get_enhancer(mode: str = "denoise") -> Enhancer:
    """Return the Modal enhancer when configured (needed for GPU-only modes), else
    the local spectral denoiser."""
    cfg = _modal_enhance_config()
    if cfg:
        return ModalEnhancer(cfg[0], cfg[1])
    return LocalEnhancer()
