"""Tests for the convert-to-wav processor (Phase 2)."""

from __future__ import annotations

import shutil
import wave
from pathlib import Path

import pytest

from orpheus_workers.ffmpeg import convert_to_wav_16k_mono
from orpheus_workers.processors.convert_to_wav import convert_to_wav

pytestmark = pytest.mark.skipif(shutil.which("ffmpeg") is None, reason="ffmpeg not installed")


def _build_wav(path: Path, seconds: int, rate: int, channels: int) -> None:
    with wave.open(str(path), "wb") as w:
        w.setnchannels(channels)
        w.setsampwidth(2)
        w.setframerate(rate)
        w.writeframes(b"\x00\x00" * rate * seconds * channels)


def test_ffmpeg_convert_produces_16k_mono(tmp_path: Path) -> None:
    src = tmp_path / "src.wav"
    dst = tmp_path / "dst.wav"
    _build_wav(src, seconds=1, rate=44100, channels=2)  # stereo, 44.1k
    convert_to_wav_16k_mono(src, dst)
    assert dst.exists()
    with wave.open(str(dst), "rb") as w:
        assert w.getframerate() == 16000
        assert w.getnchannels() == 1
        assert w.getsampwidth() == 2


class FakeDB:
    def __init__(self, src_path: Path) -> None:
        self.src_path = src_path
        self.inserted: dict | None = None

    def fetchrow(self, sql: str, *args):
        if "FROM jobs" in sql:
            return {"artifact_id": "art-src", "org_id": "org-1"}
        if "FROM artifacts" in sql:
            return {"s3_bucket": "b", "s3_key": "uploads/org-1/in.wav"}
        raise AssertionError(sql)

    def insert_artifact(self, artifact_id, org_id, bucket, key, content_type, size_bytes, *a):
        self.inserted = {
            "id": artifact_id,
            "org_id": org_id,
            "bucket": bucket,
            "key": key,
            "content_type": content_type,
            "size_bytes": size_bytes,
        }


class FakeS3:
    def __init__(self, src_path: Path) -> None:
        self.src_path = src_path
        self.uploaded: dict | None = None

    def download_file(self, bucket, key, dst):
        shutil.copyfile(self.src_path, dst)

    def upload_file(self, bucket, key, src, content_type=None):
        self.uploaded = {"bucket": bucket, "key": key, "content_type": content_type}
        return Path(src).stat().st_size


async def test_convert_to_wav_processor_end_to_end(tmp_path: Path) -> None:
    src = tmp_path / "input.wav"
    _build_wav(src, seconds=1, rate=48000, channels=2)  # 48k stereo input
    db = FakeDB(src)
    s3 = FakeS3(src)
    ctx = {"db": db, "s3": s3, "work_dir": str(tmp_path)}

    res = await convert_to_wav(ctx, "job-1")

    assert res["content_type"] == "audio/wav"
    assert res["sample_rate"] == 16000
    assert res["channels"] == 1
    assert res["source_artifact_id"] == "art-src"
    assert res["size_bytes"] > 0
    # Output artifact was registered and uploaded as wav under converted/.
    assert db.inserted is not None
    assert db.inserted["content_type"] == "audio/wav"
    assert db.inserted["key"] == "converted/org-1/art-src/audio.wav"
    assert s3.uploaded is not None
    assert s3.uploaded["content_type"] == "audio/wav"


async def test_convert_to_wav_deterministic_artifact_id(tmp_path: Path) -> None:
    src = tmp_path / "input.wav"
    _build_wav(src, seconds=1, rate=8000, channels=1)
    ctx = {"db": FakeDB(src), "s3": FakeS3(src), "work_dir": str(tmp_path)}
    first = await convert_to_wav(ctx, "job-42")
    second = await convert_to_wav(ctx, "job-42")
    assert first["artifact_id"] == second["artifact_id"], "same job must map to same artifact id"
