"""Tests for the SenseVoice analyzer selection + emotion/events processors (PRD 03)."""

from __future__ import annotations

import wave
from pathlib import Path

from orpheus_workers.processors.audio_intel import emotion_proc, events_proc
from orpheus_workers.senses import (
    ModalSenseAnalyzer,
    StubSenseAnalyzer,
    get_sense_analyzer,
)

SR = 16000


def test_default_analyzer_is_stub(monkeypatch):
    monkeypatch.delenv("ORPHEUS_SENSE_BACKEND", raising=False)
    a = get_sense_analyzer()
    assert isinstance(a, StubSenseAnalyzer)
    out = a.analyze("x.wav", [{"start": 0.0, "end": 1.0}, {"start": 1.0, "end": 2.0}])
    assert len(out) == 2
    assert out[0]["emotion"] == "neutral"
    assert out[0]["events"][0]["label"] == "Speech"


def test_modal_analyzer_selected_when_configured(monkeypatch):
    monkeypatch.setenv("ORPHEUS_SENSE_BACKEND", "modal")
    monkeypatch.setenv("ORPHEUS_MODAL_SENSE_URL", "https://s.example/analyze")
    monkeypatch.setenv("ORPHEUS_MODAL_SENSE_TOKEN", "t")
    assert isinstance(get_sense_analyzer(), ModalSenseAnalyzer)
    # missing url → falls back to stub
    monkeypatch.delenv("ORPHEUS_MODAL_SENSE_URL")
    assert isinstance(get_sense_analyzer(), StubSenseAnalyzer)


def _make_wav(path: Path, seconds: float = 2.0):
    import struct

    with wave.open(str(path), "wb") as w:
        w.setnchannels(1)
        w.setsampwidth(2)
        w.setframerate(SR)
        w.writeframes(struct.pack("<%dh" % int(seconds * SR), *([1000] * int(seconds * SR))))


class _SenseDB:
    def fetchrow(self, sql, *args):
        if "id, org_id, artifact_id, params" in sql:
            return {"id": "j1", "org_id": "org-1", "artifact_id": "art-1",
                    "params": {"source_job_id": "j0"}}
        if "SELECT result FROM jobs" in sql:
            return {"result": {
                "segments": [
                    {"start": 0.0, "end": 1.0, "text": "hello"},
                    {"start": 1.0, "end": 2.0, "text": "world"},
                ],
                "text": "hello world",
            }}
        if "SELECT s3_bucket, s3_key FROM artifacts" in sql:
            return {"s3_bucket": "b", "s3_key": "media/x.wav"}
        raise AssertionError(f"unexpected sql: {sql}")


class _SenseS3:
    def download_file(self, bucket, key, dst):
        _make_wav(Path(dst))


def _ctx(tmp_path):
    return {"db": _SenseDB(), "s3": _SenseS3(), "bucket": "b", "work_dir": str(tmp_path)}


async def test_emotion_proc_stub_labels_segments(tmp_path, monkeypatch):
    monkeypatch.delenv("ORPHEUS_SENSE_BACKEND", raising=False)
    res = await emotion_proc(_ctx(tmp_path), "j1")
    assert len(res["segments"]) == 2
    assert all(s["emotion"] == "neutral" for s in res["segments"])
    assert res["dominant_emotion"] == "neutral"
    assert res["segments"][0]["text"] == "hello"


async def test_events_proc_stub_returns_spans_and_summary(tmp_path, monkeypatch):
    monkeypatch.delenv("ORPHEUS_SENSE_BACKEND", raising=False)
    res = await events_proc(_ctx(tmp_path), "j1")
    assert res["summary"].get("Speech") == 2  # one Speech event per segment
    assert all(e["label"] == "Speech" for e in res["events"])
    assert res["events"][0]["start"] == 0.0 and res["events"][0]["end"] == 1.0


async def test_emotion_proc_degrades_when_analyzer_raises(tmp_path, monkeypatch):
    import orpheus_workers.processors.audio_intel as AI

    class Boom:
        model_version_id = "x"

        def analyze(self, wav, segments):
            raise RuntimeError("modal down")

    monkeypatch.setattr(AI, "get_sense_analyzer", lambda: Boom())
    res = await emotion_proc(_ctx(tmp_path), "j1")  # must not raise
    assert res["warnings"] == ["sense_service_unavailable"]
    assert all(s["emotion"] == "neutral" for s in res["segments"])  # stub fallback
