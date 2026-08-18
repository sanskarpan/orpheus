"""Tests for the dubbing processor (PRD 06 #367) — StubTTS, no local models."""

from __future__ import annotations

from pathlib import Path

from orpheus_workers.processors.dub import dub_proc


class _DB:
    def __init__(self):
        self.inserted = None

    def fetchrow(self, sql, *a):
        if "id, org_id, artifact_id, params" in sql:
            return {"id": "j1", "org_id": "o", "artifact_id": None, "params": self.params}
        if "SELECT result FROM jobs" in sql:
            return {"result": {"text": "hello world", "segments": [], "language": "en"}}
        if "INSERT INTO artifacts" in sql:
            self.inserted = a
            return {"id": "dub-1"}
        raise AssertionError(sql)


class _S3:
    def __init__(self):
        self.uploaded = {}

    def upload_file(self, bucket, key, src, content_type=None):
        self.uploaded[key] = Path(src).read_bytes()
        return len(self.uploaded[key])


async def test_dub_proc_synthesizes_and_writes_artifact(tmp_path, monkeypatch):
    monkeypatch.delenv("ORPHEUS_MODAL_TTS_URL", raising=False)  # → StubTTS
    db = _DB()
    db.params = {"source_job_id": "j0"}
    s3 = _S3()
    ctx = {"db": db, "s3": s3, "bucket": "b", "work_dir": str(tmp_path)}
    res = await dub_proc(ctx, "j1")
    assert res["dubbed_audio_artifact_id"] == "dub-1"
    assert res["sample_rate"] == 24000  # StubTTS rate
    assert any("dubbed-audio/o/j1.wav" in k for k in s3.uploaded)


async def test_dub_proc_translates_when_target_language(tmp_path, monkeypatch):
    monkeypatch.delenv("ORPHEUS_MODAL_TTS_URL", raising=False)
    captured = {}

    class FakeLLM:
        model_version_id = "fake"

        def translate(self, text, target, source="auto"):
            captured["target"] = target
            return f"[{target}] {text}"

    monkeypatch.setattr("orpheus_workers.processors.dub.get_llm", lambda: FakeLLM())
    db = _DB()
    db.params = {"source_job_id": "j0", "target_language": "es"}
    ctx = {"db": db, "s3": _S3(), "bucket": "b", "work_dir": str(tmp_path)}
    res = await dub_proc(ctx, "j1")
    assert captured["target"] == "es"
    assert res["target_language"] == "es"
