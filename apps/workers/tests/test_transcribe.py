import os
import tempfile
import wave
from pathlib import Path

import pytest


pytestmark = pytest.mark.skipif(
    not os.environ.get("ORPHEUS_WORKER_WHISPER_DIR") and not os.path.exists("/models"),
    reason="whisper model not installed (set ORPHEUS_WORKER_WHISPER_DIR)",
)


def _build_wav(path: Path, seconds: int, rate: int) -> None:
    with wave.open(str(path), "wb") as w:
        w.setnchannels(1)
        w.setsampwidth(2)
        w.setframerate(rate)
        w.writeframes(b"\x00\x00" * rate * seconds)


def test_transcribe_silence_returns_shape() -> None:
    """A 2-second silent wav must come back with the expected
    result shape. The transcript itself can be empty for silence;
    the point of the test is that the wrapper runs end-to-end.
    """
    with tempfile.TemporaryDirectory() as tmp:
        p = Path(tmp) / "silence.wav"
        _build_wav(p, seconds=2, rate=16000)
        from orpheus_workers.transcribe import transcribe

        result = transcribe(p, model_size="tiny.en")
    assert "text" in result
    assert "language" in result
    assert "duration_seconds" in result
    assert isinstance(result["segments"], list)
    assert isinstance(result["text"], str)
