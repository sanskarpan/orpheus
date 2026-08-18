"""Tests for audio enhancement (PRD 04): spectral denoiser + audio.enhance processor."""

from __future__ import annotations

import wave
from pathlib import Path

import numpy as np
import pytest

from orpheus_workers.enhance import LocalEnhancer, _read_wav, _rms, get_enhancer
from orpheus_workers.processors.enhance import enhance_proc

SR = 16000


def _write(path: Path, x):
    with wave.open(str(path), "wb") as w:
        w.setnchannels(1)
        w.setsampwidth(2)
        w.setframerate(SR)
        w.writeframes(np.clip(x, -32768, 32767).astype(np.int16).tobytes())


def _tone_with_noise(tmp_path) -> Path:
    t = np.arange(int(2.0 * SR)) / SR
    # 0.5s silence (noise only), 1.0s tone+noise, 0.5s silence
    tone = np.zeros_like(t)
    speech = slice(int(0.5 * SR), int(1.5 * SR))
    tone[speech] = 6000 * np.sin(2 * np.pi * 300 * t[speech])
    rng = np.random.default_rng(0)
    noisy = tone + rng.standard_normal(len(t)).astype(np.float32) * 500.0
    p = tmp_path / "noisy.wav"
    _write(p, noisy)
    return p


def test_default_enhancer_is_local(monkeypatch):
    monkeypatch.delenv("ORPHEUS_ENHANCE_BACKEND", raising=False)
    assert isinstance(get_enhancer("denoise"), LocalEnhancer)


def test_local_denoise_reduces_noise_keeps_speech(tmp_path):
    noisy = _tone_with_noise(tmp_path)
    out = tmp_path / "clean.wav"
    res = LocalEnhancer().enhance(noisy, out, "denoise")
    assert res["metrics"]["mode"] == "denoise"

    def rms(path, a, b):
        y, s = _read_wav(path)
        return _rms(y[int(a * s) : int(b * s)])

    # silent region (noise only) is suppressed; speech region largely preserved
    noise_before, noise_after = rms(noisy, 0.0, 0.4), rms(out, 0.0, 0.4)
    speech_before, speech_after = rms(noisy, 0.7, 1.3), rms(out, 0.7, 1.3)
    assert noise_after < 0.75 * noise_before  # >=25% noise removed
    assert speech_after > 0.8 * speech_before  # speech mostly kept


def test_gpu_only_mode_degrades_with_warning(tmp_path):
    noisy = _tone_with_noise(tmp_path)
    out = tmp_path / "clean.wav"
    res = LocalEnhancer().enhance(noisy, out, "isolate")  # GPU-only mode, no modal
    assert any("isolate" in w for w in res["warnings"])
    assert out.exists()  # still produced a (denoised) output


# --- processor --------------------------------------------------------------


class _EnhDB:
    def __init__(self):
        self.inserted = None

    def fetchrow(self, sql, *args):
        if "id, org_id, artifact_id, params" in sql:
            return {"id": "j1", "org_id": "org-1", "artifact_id": "art-1", "params": self.params}
        if "SELECT s3_bucket, s3_key FROM artifacts" in sql:
            return {"s3_bucket": "b", "s3_key": "media/x.wav"}
        if "INSERT INTO artifacts" in sql:
            self.inserted = args
            return {"id": "enhanced-1"}
        raise AssertionError(f"unexpected sql: {sql}")


class _EnhS3:
    def __init__(self):
        self.uploaded = {}

    def download_file(self, bucket, key, dst):
        t = np.arange(int(1.5 * SR)) / SR
        _write(Path(dst), 4000 * np.sin(2 * np.pi * 220 * t))

    def upload_file(self, bucket, key, src, content_type=None):
        self.uploaded[key] = Path(src).read_bytes()
        return len(self.uploaded[key])


async def test_enhance_proc_writes_artifact(tmp_path):
    db = _EnhDB()
    db.params = {"mode": "denoise"}
    s3 = _EnhS3()
    ctx = {"db": db, "s3": s3, "bucket": "b", "work_dir": str(tmp_path)}
    res = await enhance_proc(ctx, "j1")
    assert res["enhanced_audio_artifact_id"] == "enhanced-1"
    assert res["mode"] == "denoise"
    assert "reduction_db" in res["metrics"]
    assert any("enhanced-audio/org-1/j1.wav" in k for k in s3.uploaded)


async def test_enhance_proc_rejects_bad_mode(tmp_path):
    db = _EnhDB()
    db.params = {"mode": "bogus"}
    ctx = {"db": db, "s3": _EnhS3(), "bucket": "b", "work_dir": str(tmp_path)}
    with pytest.raises(ValueError, match="unknown enhance mode"):
        await enhance_proc(ctx, "j1")
