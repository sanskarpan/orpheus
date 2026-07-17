"""Tests for the text processors (PRD 04) with the deterministic StubLLM.

No network/model: get_llm() falls back to StubLLM when ANTHROPIC_API_KEY is
unset, so translate/summarize produce stable, assertable output. The
transcript is loaded from a prior job's result (source_job_id path).
"""

from __future__ import annotations


import pytest

from orpheus_workers.llm import StubLLM
from orpheus_workers.processors.text_ops import (
    detect_language_proc,
    summarize_proc,
    translate_proc,
)


class FakeDB:
    def __init__(self, job: dict, source_result: dict):
        self.job = job
        self.source_result = source_result

    def fetchrow(self, sql, *args):
        if "org_id, artifact_id, params" in sql:
            return self.job
        if "SELECT result FROM jobs" in sql:
            return {"result": self.source_result}
        return None


def _ctx(db):
    return {"db": db, "s3": None, "work_dir": "/tmp/orpheus-text-test", "bucket": "b"}


TRANSCRIPT = {
    "text": "hello world how are you",
    "language": "en",
    "segments": [
        {"start": 0.0, "end": 1.0, "text": "hello world", "speaker": "S1"},
        {"start": 1.0, "end": 2.0, "text": "how are you"},
    ],
}


@pytest.fixture(autouse=True)
def _no_api_key(monkeypatch):
    monkeypatch.delenv("ANTHROPIC_API_KEY", raising=False)


async def test_stub_llm_deterministic():
    llm = StubLLM()
    assert llm.translate("hola", "en") == "[en] hola"
    assert llm.summarize("a b c d", mode="bullets").startswith("[bullets] ")
    assert llm.detect_language("the cat sat")[0] == "en"


async def test_detect_language_uses_whisper_when_present():
    db = FakeDB({"org_id": "o", "artifact_id": None, "params": {"source_job_id": "j0"}}, TRANSCRIPT)
    res = await detect_language_proc(_ctx(db), "j1")
    assert res["language"] == "en"
    assert res["model_version_id"] == "whisper-detect"


async def test_translate_preserves_timestamps_and_speakers():
    db = FakeDB(
        {
            "org_id": "o",
            "artifact_id": None,
            "params": {"source_job_id": "j0", "target_language": "es"},
        },
        TRANSCRIPT,
    )
    res = await translate_proc(_ctx(db), "j1")
    assert res["target_language"] == "es"
    assert len(res["segments"]) == 2
    # Timestamps preserved; text translated (stub marker); speaker carried over.
    assert res["segments"][0] == {
        "start": 0.0,
        "end": 1.0,
        "text": "[es] hello world",
        "speaker": "S1",
    }
    assert res["segments"][1] == {"start": 1.0, "end": 2.0, "text": "[es] how are you"}
    assert res["text"] == "[es] hello world [es] how are you"
    assert res["model_version_id"] == "stub-llm-1"


async def test_translate_requires_target():
    db = FakeDB({"org_id": "o", "artifact_id": None, "params": {"source_job_id": "j0"}}, TRANSCRIPT)
    with pytest.raises(ValueError):
        await translate_proc(_ctx(db), "j1")


async def test_summarize_produces_summary():
    db = FakeDB(
        {"org_id": "o", "artifact_id": None, "params": {"source_job_id": "j0", "mode": "bullets"}},
        TRANSCRIPT,
    )
    res = await summarize_proc(_ctx(db), "j1")
    assert res["mode"] == "bullets"
    assert res["summary"].startswith("[bullets] ")
    assert "hello world" in res["summary"]


async def test_summarize_rejects_empty():
    db = FakeDB(
        {"org_id": "o", "artifact_id": None, "params": {"source_job_id": "j0"}},
        {"text": "   ", "segments": []},
    )
    with pytest.raises(ValueError):
        await summarize_proc(_ctx(db), "j1")
