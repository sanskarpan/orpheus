"""Tests for audio-intelligence processors (PRD 13): chapters + moderation."""

from __future__ import annotations

from orpheus_workers.processors.audio_intel import (
    ProfanityDetector,
    _build_chapters,
    _mask_profanity,
    _merge_ranges,
    _pii_time_ranges,
    audio_redact_proc,
    chapters_proc,
    moderate_proc,
)
from orpheus_workers.redact import RegexDetector

SEGMENTS = [
    {"start": 0.0, "end": 5.0, "text": "welcome to the show"},
    {"start": 5.0, "end": 10.0, "text": "today we discuss pricing"},
    {"start": 10.0, "end": 15.0, "text": "now onto the roadmap"},
    {"start": 15.0, "end": 20.0, "text": "and finally questions"},
]


class FakeDB:
    def __init__(self, transcript, params):
        self.transcript = transcript
        self.params = params

    def fetchrow(self, sql, *args):
        if "org_id, artifact_id, params" in sql:
            return {"org_id": "org-1", "artifact_id": None, "params": self.params}
        if "SELECT result FROM jobs" in sql:
            return {"result": self.transcript}
        raise AssertionError(f"unexpected sql: {sql}")


def _ctx(transcript, params, tmp_path):
    return {"db": FakeDB(transcript, params), "s3": None, "work_dir": str(tmp_path)}


# --- _build_chapters --------------------------------------------------------


def test_build_chapters_maps_indices_to_timestamps():
    data = {
        "chapters": [
            {"start_index": 0, "title": "Intro", "summary": "hi"},
            {"start_index": 2, "title": "Roadmap", "summary": "plans"},
        ]
    }
    chs = _build_chapters(data, SEGMENTS)
    assert [c["title"] for c in chs] == ["Intro", "Roadmap"]
    assert chs[0]["start"] == 0.0 and chs[0]["end"] == 10.0  # runs to seg before next
    assert chs[1]["start"] == 10.0 and chs[1]["end"] == 20.0  # last → final seg end


def test_build_chapters_forces_start_at_zero_and_dedupes():
    data = {
        "chapters": [
            {"start_index": 2, "title": "B"},
            {"start_index": 2, "title": "dup"},  # deduped
        ]
    }
    chs = _build_chapters(data, SEGMENTS)
    assert chs[0]["start"] == 0.0  # a chapter is forced at segment 0
    assert len(chs) == 2 and chs[0]["title"] == "Introduction"


def test_build_chapters_fallback_on_garbage():
    assert _build_chapters({"raw": "not json"}, SEGMENTS)[0]["start"] == 0.0
    # empty segments → single whole-file chapter
    out = _build_chapters({"chapters": []}, [])
    assert len(out) == 1 and out[0]["title"] == "Full transcript"


def test_build_chapters_clamps_out_of_range_index():
    chs = _build_chapters({"chapters": [{"start_index": 99, "title": "X"}]}, SEGMENTS)
    assert all(0.0 <= c["start"] <= 20.0 for c in chs)


# --- profanity --------------------------------------------------------------


def test_profanity_detector_whole_word_case_insensitive():
    spans = ProfanityDetector({"damn"}).detect("Damn this damnation", ["PROFANITY"])
    assert len(spans) == 1  # "Damn" matches, "damnation" does not (whole word)
    assert spans[0].text == "Damn"


def test_mask_profanity_modes():
    masked, n = _mask_profanity("this is crap", {"crap"}, "type")
    assert masked == "this is [PROFANITY]" and n == 1
    masked_c, _ = _mask_profanity("this is crap", {"crap"}, "char")
    assert "●" in masked_c and "crap" not in masked_c


def test_mask_profanity_clean_control_zero_mutes():
    masked, n = _mask_profanity("a perfectly clean sentence", {"crap"}, "type")
    assert n == 0 and masked == "a perfectly clean sentence"


# --- processors -------------------------------------------------------------


async def test_chapters_proc_with_stub_llm_returns_valid_structure(tmp_path):
    tr = {"text": "x", "segments": SEGMENTS, "language": "en"}
    ctx = _ctx(tr, {"source_job_id": "j0"}, tmp_path)
    res = await chapters_proc(ctx, "j1")
    assert res["chapters"] and res["chapters"][0]["start"] == 0.0  # stub → fallback still valid
    assert "model_version_id" in res


async def test_chapters_proc_uses_llm_boundaries(tmp_path, monkeypatch):
    class FakeLLM:
        model_version_id = "fake-1"

        def complete(self, system, user, max_tokens=512):
            return '{"chapters":[{"start_index":0,"title":"Intro"},{"start_index":2,"title":"Roadmap"}]}'

    monkeypatch.setattr("orpheus_workers.processors.audio_intel.get_llm", lambda: FakeLLM())
    tr = {"text": "x", "segments": SEGMENTS, "language": "en"}
    res = await chapters_proc(_ctx(tr, {"source_job_id": "j0"}, tmp_path), "j1")
    assert [c["title"] for c in res["chapters"]] == ["Intro", "Roadmap"]
    assert res["chapters"][1]["start"] == 10.0


async def test_moderate_proc_lexicon_flags_and_masks(tmp_path):
    tr = {"text": "this is total crap and a bitch to use", "segments": [], "language": "en"}
    ctx = _ctx(tr, {"source_job_id": "j0", "mask_profanity": True}, tmp_path)
    res = await moderate_proc(ctx, "j1")
    assert res["flagged"] is True
    assert res["profanity"]["count"] == 2
    assert "crap" not in res["profanity"]["masked_text"]
    assert "[PROFANITY]" in res["profanity"]["masked_text"]


async def test_moderate_proc_clean_text_not_flagged(tmp_path):
    tr = {"text": "a friendly and professional conversation", "segments": [], "language": "en"}
    res = await moderate_proc(_ctx(tr, {"source_job_id": "j0"}, tmp_path), "j1")
    assert res["flagged"] is False and res["profanity"]["count"] == 0


# --- audio PII beep redaction -----------------------------------------------


def test_merge_ranges_sorts_and_merges():
    assert _merge_ranges([(3.0, 4.0), (0.0, 1.0), (1.02, 2.0)]) == [(0.0, 2.0), (3.0, 4.0)]
    assert _merge_ranges([(1.0, 1.0), (2.0, 1.5)]) == []  # zero/negative dropped


def test_pii_time_ranges_word_level():
    tr = {
        "segments": [
            {
                "start": 0.0,
                "end": 2.0,
                "text": "email alice@example.com now",
                "words": [
                    {"word": "email", "start": 0.0, "end": 0.4},
                    {"word": "alice@example.com", "start": 0.4, "end": 1.5},
                    {"word": "now", "start": 1.5, "end": 2.0},
                ],
            }
        ]
    }
    ranges, counts, coarse = _pii_time_ranges(tr, RegexDetector(), ["EMAIL"], pad=0.0)
    assert counts == {"EMAIL": 1} and coarse is False
    assert ranges == [(0.4, 1.5)]  # only the email word muted, not "email"/"now"
    # with the default guard band the range is padded on both edges
    padded, _, _ = _pii_time_ranges(tr, RegexDetector(), ["EMAIL"])
    (s, e) = padded[0]
    assert abs(s - 0.28) < 1e-6 and abs(e - 1.62) < 1e-6


def test_pii_time_ranges_coarse_fallback_without_words():
    tr = {"segments": [{"start": 5.0, "end": 8.0, "text": "call 415-555-1234 today"}]}
    ranges, counts, coarse = _pii_time_ranges(tr, RegexDetector(), ["PHONE"])
    assert counts == {"PHONE": 1} and coarse is True
    assert ranges == [(5.0, 8.0)]  # whole segment muted (no word timings)


class _RedactDB:
    def __init__(self, transcript):
        self.transcript = transcript
        self.inserted = []

    def fetchrow(self, sql, *args):
        if "org_id, artifact_id, params" in sql:
            return {"org_id": "org-1", "artifact_id": "art-1", "params": {"source_job_id": "j0"}}
        if "SELECT result FROM jobs" in sql:
            return {"result": self.transcript}
        if "SELECT s3_bucket, s3_key FROM artifacts" in sql:
            return {"s3_bucket": "b", "s3_key": "media/x.wav"}
        if "INSERT INTO artifacts" in sql:
            self.inserted.append(args)
            return {"id": "redacted-audio-1"}
        raise AssertionError(f"unexpected sql: {sql}")


class _RedactS3:
    def __init__(self):
        self.uploaded = {}

    def download_file(self, bucket, key, dst):
        from pathlib import Path

        Path(dst).write_bytes(b"RIFFfake-wav-bytes")

    def upload_file(self, bucket, key, src, content_type=None):
        from pathlib import Path

        self.uploaded[key] = Path(src).read_bytes()
        return len(self.uploaded[key])


async def test_audio_redact_proc_mutes_pii_and_writes_artifact(tmp_path, monkeypatch):
    import orpheus_workers.processors.audio_intel as AI

    captured = {}

    def fake_mute(src, dst, ranges):
        from pathlib import Path

        captured["ranges"] = ranges
        Path(dst).write_bytes(b"RIFFmuted")

    monkeypatch.setattr(AI, "mute_ranges", fake_mute)

    tr = {
        "segments": [
            {
                "start": 0.0,
                "end": 2.0,
                "text": "reach me at bob@corp.com please",
                "words": [
                    {"word": "reach", "start": 0.0, "end": 0.3},
                    {"word": "me", "start": 0.3, "end": 0.5},
                    {"word": "at", "start": 0.5, "end": 0.7},
                    {"word": "bob@corp.com", "start": 0.7, "end": 1.6},
                    {"word": "please", "start": 1.6, "end": 2.0},
                ],
            }
        ],
        "text": "reach me at bob@corp.com please",
    }
    db = _RedactDB(tr)
    s3 = _RedactS3()
    ctx = {"db": db, "s3": s3, "bucket": "b", "work_dir": str(tmp_path)}
    res = await audio_redact_proc(ctx, "j1")

    assert res["redacted_audio_artifact_id"] == "redacted-audio-1"
    assert res["redactions"] == [{"entity": "EMAIL", "count": 1}]
    # only the email word range muted, padded by the guard band (0.12s each side)
    assert res["muted_ranges"] == [{"start": 0.58, "end": 1.72}]
    assert len(captured["ranges"]) == 1
    assert any("redacted-audio/org-1/j1.wav" in k for k in s3.uploaded)
    assert len(db.inserted) == 1
