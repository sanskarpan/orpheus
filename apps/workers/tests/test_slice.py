import shutil
import tempfile
import wave
from pathlib import Path

import pytest

from orpheus_workers.ffmpeg import slice as run_ffmpeg_slice


pytestmark = pytest.mark.skipif(shutil.which("ffmpeg") is None, reason="ffmpeg not installed")


def _build_wav(path: Path, seconds: int, rate: int) -> None:
    with wave.open(str(path), "wb") as w:
        w.setnchannels(1)
        w.setsampwidth(2)
        w.setframerate(rate)
        w.writeframes(b"\x00\x00" * rate * seconds)


def test_ffmpeg_slice_writes_shorter_wav() -> None:
    with tempfile.TemporaryDirectory() as tmp:
        src = Path(tmp) / "src.wav"
        dst = Path(tmp) / "dst.wav"
        _build_wav(src, seconds=2, rate=8000)
        run_ffmpeg_slice(src, dst, start_seconds=0.5, end_seconds=1.5)
        assert dst.exists()
        size = dst.stat().st_size
        assert 8000 < size < 24000, f"unexpected slice size: {size}"


def test_ffmpeg_slice_rejects_inverted_range() -> None:
    with tempfile.TemporaryDirectory() as tmp:
        src = Path(tmp) / "src.wav"
        dst = Path(tmp) / "dst.wav"
        _build_wav(src, seconds=2, rate=8000)
        with pytest.raises(Exception):
            run_ffmpeg_slice(src, dst, start_seconds=2.0, end_seconds=1.0)
