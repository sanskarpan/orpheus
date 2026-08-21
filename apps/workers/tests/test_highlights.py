"""Tests for the highlight-reel processor (PRD 16 #361)."""

from __future__ import annotations

from orpheus_workers.processors.highlights import _plan_highlights, highlights_proc

SEGMENTS = [
    {"start": 0.0, "end": 5.0, "text": "intro"},
    {"start": 5.0, "end": 10.0, "text": "we decided to ship on friday"},
    {"start": 10.0, "end": 15.0, "text": "the customer loved the demo"},
    {"start": 15.0, "end": 20.0, "text": "wrap up"},
]


def test_plan_highlights_maps_ranges():
    data = {
        "highlights": [
            {"start_index": 1, "end_index": 1, "title": "Ship date", "reason": "decision"},
            {"start_index": 2, "end_index": 2, "title": "Customer reaction", "reason": "positive"},
        ]
    }
    hl = _plan_highlights(data, SEGMENTS)
    assert [h["title"] for h in hl] == ["Ship date", "Customer reaction"]
    assert hl[0]["start"] == 5.0 and hl[0]["end"] == 10.0
    assert hl[1]["start"] == 10.0 and hl[1]["end"] == 15.0


def test_plan_highlights_clamps_dedupes_caps():
    data = {
        "highlights": [
            {"start_index": 99, "end_index": 99, "title": "oob"},  # clamped to last
            {"start_index": 1, "end_index": 1, "title": "a"},
            {"start_index": 1, "end_index": 1, "title": "dup"},  # deduped
        ]
    }
    hl = _plan_highlights(data, SEGMENTS, max_n=2)
    assert len(hl) == 2
    assert all(0.0 <= h["start"] <= 20.0 for h in hl)


def test_plan_highlights_empty():
    assert _plan_highlights({"raw": "x"}, SEGMENTS) == []
    assert _plan_highlights({"highlights": []}, []) == []


class _DB:
    def __init__(self, tr):
        self.tr = tr

    def fetchrow(self, sql, *a):
        if "id, org_id, artifact_id, params" in sql:
            return {
                "id": "j1",
                "org_id": "o",
                "artifact_id": None,
                "params": {"source_job_id": "j0"},
            }
        if "SELECT result FROM jobs" in sql:
            return {"result": self.tr}
        raise AssertionError(sql)


async def test_highlights_proc_stub_returns_structure(tmp_path):
    # StubLLM → _analyze_json returns {"raw":...} → fallback empty, but valid shape.
    ctx = {
        "db": _DB({"segments": SEGMENTS, "text": "x"}),
        "s3": None,
        "bucket": "b",
        "work_dir": str(tmp_path),
    }
    res = await highlights_proc(ctx, "j1")
    assert "highlights" in res and isinstance(res["highlights"], list)
    assert "model_version_id" in res


async def test_highlights_proc_uses_llm(tmp_path, monkeypatch):
    class FakeLLM:
        model_version_id = "fake"

        def complete(self, system, user, max_tokens=512):
            return '{"highlights":[{"start_index":1,"end_index":2,"title":"Big moment","reason":"key"}]}'

    monkeypatch.setattr("orpheus_workers.processors.highlights.get_llm", lambda: FakeLLM())
    ctx = {
        "db": _DB({"segments": SEGMENTS, "text": "x"}),
        "s3": None,
        "bucket": "b",
        "work_dir": str(tmp_path),
    }
    res = await highlights_proc(ctx, "j1")
    assert len(res["highlights"]) == 1
    assert res["highlights"][0]["title"] == "Big moment"
    assert res["highlights"][0]["start"] == 5.0 and res["highlights"][0]["end"] == 15.0
