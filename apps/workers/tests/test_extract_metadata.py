import wave
from pathlib import Path

from orpheus_workers.processors.extract_metadata import _extract_from_path


def test_extract_from_path_wav() -> None:
    """Build a 1s mono 8kHz silent wav, run mutagen, assert the shape."""
    import tempfile

    with tempfile.TemporaryDirectory() as tmp:
        p = Path(tmp) / "sample.wav"
        with wave.open(str(p), "wb") as w:
            w.setnchannels(1)
            w.setsampwidth(2)
            w.setframerate(8000)
            w.writeframes(b"\x00\x00" * 8000)

        result = _extract_from_path(str(p))

    assert result["format"] == "audio/wav"
    assert result["sample_rate"] == 8000
    assert result["channels"] == 1
    assert result["duration_seconds"] is not None
    assert 0.9 < result["duration_seconds"] < 1.1
    assert result["bitrate"] is not None
    assert isinstance(result["tags"], dict)
