"""Tests for PRD 05: subtitle builders, diarization assignment, subtitle export.

Pure builders are tested directly; the diarizer uses the deterministic stub
(no pyannote/torch); ffmpeg conversion is monkeypatched to a copy so the tests
are hermetic.
"""

from __future__ import annotations

import wave
from pathlib import Path

import pytest

from orpheus_workers.diarize import StubDiarizer, get_diarizer
from orpheus_workers.processors import audio_ops
from orpheus_workers.processors.audio_ops import (
    build_srt,
    build_vtt,
    diarize_proc,
    export_subtitles_proc,
)

SEGMENTS = [
    {"start": 0.0, "end": 3.0, "text": "hello there"},
    {"start": 3.0, "end": 8.0, "text": "general kenobi you are a bold one"},
]


def _make_wav(path: Path, seconds: float, rate: int = 16000) -> None:
    with wave.open(str(path), "wb") as w:
        w.setnchannels(1)
        w.setsampwidth(2)
        w.setframerate(rate)
        w.writeframes(b"\x00\x00" * int(rate * seconds))


# --- pure builders ----------------------------------------------------------


def test_build_srt_has_indexed_cues_and_speakers():
    segs = [dict(s, speaker="S1") for s in SEGMENTS]
    srt = build_srt(segs, include_speaker=True, max_chars=42, max_lines=2)
    assert srt.startswith("1\n00:00:00,000 --> 00:00:03,000\nS1: hello there")
    assert "2\n00:00:03,000 --> 00:00:08,000\nS1: general kenobi" in srt


def test_build_vtt_header_and_escaping():
    segs = [{"start": 0.0, "end": 1.0, "text": "a < b & c"}]
    vtt = build_vtt(segs, include_speaker=False, max_chars=80, max_lines=2)
    assert vtt.startswith("WEBVTT")
    assert "00:00:00.000 --> 00:00:01.000" in vtt
    assert "a &lt; b &amp; c" in vtt  # sanitized


def test_wrap_limits_lines():
    long = {"start": 0.0, "end": 2.0, "text": " ".join(["word"] * 40)}
    srt = build_srt([long], include_speaker=False, max_chars=20, max_lines=2)
    body = srt.split("\n", 2)[2]
    cue_lines = [ln for ln in body.splitlines() if ln.strip()]
    assert len(cue_lines) <= 2


# --- diarizer ---------------------------------------------------------------


def test_stub_diarizer_covers_duration(tmp_path):
    wav = tmp_path / "a.wav"
    _make_wav(wav, 8.0)
    turns = StubDiarizer(window_seconds=5.0, num_speakers=2).diarize(wav)
    assert turns[0] == {"start": 0.0, "end": 5.0, "speaker": "S1"}
    assert turns[1]["speaker"] == "S2"
    assert abs(turns[-1]["end"] - 8.0) < 0.01


def test_get_diarizer_defaults_to_stub(monkeypatch):
    monkeypatch.delenv("ORPHEUS_DIARIZE_MODEL", raising=False)
    assert isinstance(get_diarizer(), StubDiarizer)


# --- processors -------------------------------------------------------------


class FakeDB:
    def __init__(self, job, transcript, insert_ids=None):
        self.job = job
        self.transcript = transcript
        self.insert_ids = list(insert_ids or [])
        self.inserted = []

    def fetchrow(self, sql, *args):
        if "org_id, artifact_id, params" in sql:
            return self.job
        if "SELECT result FROM jobs" in sql:
            return {"result": self.transcript}
        if "FROM artifacts WHERE id" in sql:
            return {"s3_bucket": "b", "s3_key": "k/audio.wav"}
        if "INSERT INTO artifacts" in sql:
            self.inserted.append(args)
            return {"id": self.insert_ids.pop(0) if self.insert_ids else "art-new"}
        raise AssertionError(f"unexpected sql: {sql}")


class FakeS3:
    def __init__(self, wav_seconds=8.0):
        self.wav_seconds = wav_seconds
        self.uploaded = {}

    def download_file(self, bucket, key, dest):
        _make_wav(Path(dest), self.wav_seconds)

    def upload_file(self, bucket, key, src, content_type=None):
        self.uploaded[key] = Path(src).read_bytes()
        return len(self.uploaded[key])


async def test_diarize_proc_assigns_speakers(tmp_path, monkeypatch):
    # Avoid ffmpeg: convert = copy.
    monkeypatch.setattr(
        audio_ops,
        "convert_to_wav_16k_mono",
        lambda src, dst: Path(dst).write_bytes(Path(src).read_bytes()),
    )
    monkeypatch.delenv("ORPHEUS_DIARIZE_MODEL", raising=False)

    transcript = {"text": "hello there general kenobi", "language": "en", "segments": SEGMENTS}
    job = {
        "org_id": "o",
        "artifact_id": "aud-1",
        "params": {"source_job_id": "j0", "max_speakers": 2},
    }
    db = FakeDB(job, transcript)
    s3 = FakeS3(wav_seconds=8.0)
    ctx = {"db": db, "s3": s3, "bucket": "b", "work_dir": str(tmp_path)}

    res = await diarize_proc(ctx, "j1")
    assert res["num_speakers"] == 2
    # seg 0-3 falls in the first 5s window (S1); seg 3-8 overlaps S2 more.
    assert res["segments"][0]["speaker"] == "S1"
    assert res["segments"][1]["speaker"] == "S2"
    assert res["model_version_id"] == "stub-diarize-1"


async def test_export_subtitles_proc_writes_artifacts(tmp_path):
    transcript = {"text": "hi", "segments": [dict(s, speaker="S1") for s in SEGMENTS]}
    job = {
        "org_id": "o",
        "artifact_id": None,
        "params": {"source_job_id": "j0", "formats": ["srt", "vtt"]},
    }
    db = FakeDB(job, transcript, insert_ids=["art-srt", "art-vtt"])
    s3 = FakeS3()
    ctx = {"db": db, "s3": s3, "bucket": "b", "work_dir": str(tmp_path)}

    res = await export_subtitles_proc(ctx, "j1")
    assert set(res["formats"]) == {"srt", "vtt"}
    assert len(res["artifacts"]) == 2
    # Both files were uploaded + artifact rows inserted.
    assert len(db.inserted) == 2
    srt_key = "subtitles/o/j1.srt"
    assert srt_key in s3.uploaded
    assert b"S1: hello there" in s3.uploaded[srt_key]
    assert s3.uploaded["subtitles/o/j1.vtt"].startswith(b"WEBVTT")


async def test_export_subtitles_rejects_unknown_format(tmp_path):
    transcript = {"text": "hi", "segments": SEGMENTS}
    job = {
        "org_id": "o",
        "artifact_id": None,
        "params": {"source_job_id": "j0", "formats": ["ass"]},
    }
    db = FakeDB(job, transcript)
    ctx = {"db": db, "s3": FakeS3(), "bucket": "b", "work_dir": str(tmp_path)}
    with pytest.raises(ValueError):
        await export_subtitles_proc(ctx, "j1")
