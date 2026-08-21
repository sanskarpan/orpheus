"""Tests for audio-edit-by-text / filler removal (PRD 16 #365)."""

from __future__ import annotations

import wave
from pathlib import Path

import numpy as np

from orpheus_workers.ffmpeg import cut_ranges
from orpheus_workers.processors.edit import _DEFAULT_FILLERS, edit_proc, plan_filler_edit

SR = 16000


def _tr():
    # "hello um world uh again" — 'um' and 'uh' are fillers to remove
    words = [
        {"word": "hello", "start": 0.0, "end": 0.5},
        {"word": "um", "start": 0.5, "end": 1.0},
        {"word": "world", "start": 1.0, "end": 1.5},
        {"word": "uh", "start": 1.5, "end": 2.0},
        {"word": "again", "start": 2.0, "end": 2.5},
    ]
    return {
        "segments": [{"start": 0.0, "end": 2.5, "text": "hello um world uh again", "words": words}]
    }


def test_plan_filler_edit_drops_and_reindexes():
    ranges, cleaned, removed = plan_filler_edit(_tr(), _DEFAULT_FILLERS, pad=0.0)
    assert removed == 2
    assert ranges == [(0.5, 1.0), (1.5, 2.0)]  # the two filler spans
    kept = cleaned[0]["words"]
    assert [w["word"] for w in kept] == ["hello", "world", "again"]
    # timestamps re-indexed onto the shortened timeline (each filler = 0.5s cut)
    assert kept[0]["start"] == 0.0 and kept[0]["end"] == 0.5  # hello unchanged
    assert kept[1]["start"] == 0.5 and kept[1]["end"] == 1.0  # world shifted -0.5
    assert kept[2]["start"] == 1.0 and kept[2]["end"] == 1.5  # again shifted -1.0
    assert cleaned[0]["text"] == "hello world again"


def test_plan_filler_edit_no_fillers():
    tr = {
        "segments": [
            {
                "start": 0,
                "end": 1,
                "text": "clean speech",
                "words": [
                    {"word": "clean", "start": 0, "end": 0.5},
                    {"word": "speech", "start": 0.5, "end": 1.0},
                ],
            }
        ]
    }
    ranges, cleaned, removed = plan_filler_edit(tr, _DEFAULT_FILLERS)
    assert removed == 0 and ranges == []
    assert cleaned[0]["text"] == "clean speech"


def test_cut_ranges_shortens_audio(tmp_path):
    # 3s tone; cut out [1.0,2.0] → ~2s output
    t = np.arange(3 * SR) / SR
    tone = (np.sin(2 * np.pi * 220 * t) * 8000).astype(np.int16)
    src = tmp_path / "in.wav"
    with wave.open(str(src), "wb") as w:
        w.setnchannels(1)
        w.setsampwidth(2)
        w.setframerate(SR)
        w.writeframes(tone.tobytes())
    out = tmp_path / "out.wav"
    cut_ranges(src, out, [(1.0, 2.0)])
    with wave.open(str(out), "rb") as w:
        dur = w.getnframes() / w.getframerate()
    assert 1.9 < dur < 2.1  # one second removed


class _EditDB:
    def __init__(self, transcript):
        self.transcript = transcript
        self.inserted = None

    def fetchrow(self, sql, *args):
        if "id, org_id, artifact_id, params" in sql:
            return {
                "id": "j1",
                "org_id": "org-1",
                "artifact_id": "art-1",
                "params": {"source_job_id": "j0", "mode": "remove_fillers"},
            }
        if "SELECT result FROM jobs" in sql:
            return {"result": self.transcript}
        if "SELECT s3_bucket, s3_key FROM artifacts" in sql:
            return {"s3_bucket": "b", "s3_key": "media/x.wav"}
        if "INSERT INTO artifacts" in sql:
            self.inserted = args
            return {"id": "edited-1"}
        raise AssertionError(f"unexpected sql: {sql}")


class _EditS3:
    def __init__(self):
        self.uploaded = {}

    def download_file(self, bucket, key, dst):
        t = np.arange(int(2.5 * SR)) / SR
        with wave.open(str(dst), "wb") as w:
            w.setnchannels(1)
            w.setsampwidth(2)
            w.setframerate(SR)
            w.writeframes((np.sin(2 * np.pi * 200 * t) * 6000).astype(np.int16).tobytes())

    def upload_file(self, bucket, key, src, content_type=None):
        self.uploaded[key] = Path(src).read_bytes()
        return len(self.uploaded[key])


async def test_edit_proc_removes_fillers_and_writes_artifact(tmp_path):
    db = _EditDB(_tr())
    s3 = _EditS3()
    ctx = {"db": db, "s3": s3, "bucket": "b", "work_dir": str(tmp_path)}
    res = await edit_proc(ctx, "j1")
    assert res["edited_audio_artifact_id"] == "edited-1"
    assert res["fillers_removed"] == 2
    assert res["text"] == "hello world again"
    assert any("edited-audio/org-1/j1.wav" in k for k in s3.uploaded)
