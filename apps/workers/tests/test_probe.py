import shutil
import tempfile
import wave
from pathlib import Path
from unittest.mock import MagicMock, patch

import pytest

from orpheus_workers.ffprobe import extract_audio_metadata, probe
from orpheus_workers.processors.probe import probe_artifact

FFPROBE_REQUIRED = pytest.mark.skipif(
    shutil.which("ffprobe") is None, reason="ffprobe not installed"
)


def _build_wav(path: Path, seconds: int = 1, rate: int = 8000) -> None:
    with wave.open(str(path), "wb") as w:
        w.setnchannels(1)
        w.setsampwidth(2)
        w.setframerate(rate)
        w.writeframes(b"\x00\x00" * rate * seconds)


@FFPROBE_REQUIRED
def test_probe_wav_extracts_metadata() -> None:
    with tempfile.TemporaryDirectory() as tmp:
        p = Path(tmp) / "test.wav"
        _build_wav(p, seconds=1, rate=8000)
        data = probe(p)
        meta = extract_audio_metadata(data)
    assert meta["codec"] == "pcm_s16le"
    assert meta["sample_rate"] == 8000
    assert meta["channels"] == 1
    assert meta["duration_seconds"] is not None
    assert 0.9 < meta["duration_seconds"] < 1.1


def test_extract_audio_metadata_no_audio_stream_returns_nones() -> None:
    data = {"streams": [{"codec_type": "video", "codec_name": "h264"}], "format": {}}
    meta = extract_audio_metadata(data)
    assert meta == {
        "codec": None,
        "sample_rate": None,
        "channels": None,
        "duration_seconds": None,
    }


async def test_probe_artifact_no_audio_stream_marks_artifact_failed() -> None:
    """Regression: when ffprobe returns no audio stream, the artifact
    must be marked probe_status='failed' before the exception
    propagates to the worker (which then marks the job failed).
    """
    db = MagicMock()
    db.fetchrow.side_effect = [
        {"artifact_id": "art-1"},
        {"s3_bucket": "b", "s3_key": "k.wav"},
    ]
    s3 = MagicMock()
    with tempfile.TemporaryDirectory() as tmp:
        with patch("orpheus_workers.processors.probe.run_ffprobe") as run:
            run.return_value = {"streams": [], "format": {}}
            with pytest.raises(Exception, match="no audio stream"):
                await probe_artifact({"db": db, "s3": s3, "work_dir": tmp}, "job-1")
    db.mark_artifact_probe_failed.assert_called_once_with("art-1")
    db.mark_artifact_probed.assert_not_called()
