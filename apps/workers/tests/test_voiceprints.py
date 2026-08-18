"""Tests for speaker voiceprints (PRD 03 §4.8): embed, match, enroll, identify."""

from __future__ import annotations

import wave
from pathlib import Path

import pytest

from orpheus_workers.processors.audio_ops import _identify_speakers
from orpheus_workers.processors.speakers import enroll_proc
from orpheus_workers.voiceprints import (
    StubEmbedder,
    cosine,
    get_embedder,
    match_profile,
)

SR = 16000


def _make_wav(path: Path, seed: int = 7, seconds: float = 2.0):
    import struct

    n = int(seconds * SR)
    samples = [(seed * (i + 1)) % 2000 - 1000 for i in range(n)]
    with wave.open(str(path), "wb") as w:
        w.setnchannels(1)
        w.setsampwidth(2)
        w.setframerate(SR)
        w.writeframes(struct.pack("<%dh" % n, *samples))


def test_default_embedder_is_stub_and_unit_norm(monkeypatch):
    monkeypatch.delenv("ORPHEUS_DIARIZE_BACKEND", raising=False)
    e = get_embedder()
    assert isinstance(e, StubEmbedder)


def test_stub_embedding_stable_across_rewrite(tmp_path):
    a = tmp_path / "a.wav"
    _make_wav(a, seed=3)
    v1 = StubEmbedder().embed(a)
    # a clip re-written with the same PCM samples embeds identically
    b = tmp_path / "b.wav"
    with wave.open(str(a), "rb") as r:
        frames = r.readframes(r.getnframes())
    with wave.open(str(b), "wb") as w:
        w.setnchannels(1)
        w.setsampwidth(2)
        w.setframerate(SR)
        w.writeframes(frames)
    assert StubEmbedder().embed(b) == v1
    assert abs(sum(x * x for x in v1) - 1.0) < 1e-6  # unit norm


def test_match_profile_threshold_and_dim_guard():
    emb = [1.0, 0.0]
    profs = [
        {"id": "1", "name": "Alice", "embedding": [1.0, 0.0]},
        {"id": "2", "name": "Bob", "embedding": [0.0, 1.0]},
        {"id": "3", "name": "WrongDim", "embedding": [1.0, 0.0, 0.0]},  # skipped
    ]
    m = match_profile(emb, profs, threshold=0.5)
    assert m["name"] == "Alice" and m["score"] == 1.0
    # below threshold → no match
    assert match_profile([0.0, 1.0], profs[:1], threshold=0.5) is None


def test_cosine_guards():
    assert cosine([], [1.0]) == -1.0
    assert cosine([1.0, 0.0], [1.0, 0.0]) == 1.0


# --- enroll processor -------------------------------------------------------


class _EnrollDB:
    def __init__(self):
        self.inserted = None

    def fetchrow(self, sql, *args):
        if "id, org_id, artifact_id, params" in sql:
            return {"id": "j1", "org_id": "org-1", "artifact_id": "art-1", "params": self.params}
        if "SELECT s3_bucket, s3_key FROM artifacts" in sql:
            return {"s3_bucket": "b", "s3_key": "media/x.wav"}
        if "INSERT INTO speaker_profiles" in sql:
            self.inserted = args
            return {"id": "spk-1"}
        raise AssertionError(f"unexpected sql: {sql}")


class _EnrollS3:
    def download_file(self, bucket, key, dst):
        _make_wav(Path(dst), seed=5)


async def test_enroll_requires_consent(tmp_path):
    db = _EnrollDB()
    db.params = {"name": "Alice", "consent": False}
    ctx = {"db": db, "s3": _EnrollS3(), "bucket": "b", "work_dir": str(tmp_path)}
    with pytest.raises(ValueError, match="consent"):
        await enroll_proc(ctx, "j1")


async def test_enroll_requires_name(tmp_path):
    db = _EnrollDB()
    db.params = {"consent": True}
    ctx = {"db": db, "s3": _EnrollS3(), "bucket": "b", "work_dir": str(tmp_path)}
    with pytest.raises(ValueError, match="name"):
        await enroll_proc(ctx, "j1")


async def test_enroll_inserts_voiceprint(tmp_path):
    db = _EnrollDB()
    db.params = {"name": "Alice", "consent": True}
    ctx = {"db": db, "s3": _EnrollS3(), "bucket": "b", "work_dir": str(tmp_path)}
    res = await enroll_proc(ctx, "j1")
    assert res["speaker_id"] == "spk-1" and res["name"] == "Alice"
    # INSERT args: (org_id, name, embedding, dim, model_version_id)
    org_id, name, embedding, dim, _mv = db.inserted
    assert org_id == "org-1" and name == "Alice"
    assert dim == len(embedding) == StubEmbedder.dim


# --- identify (recognition) -------------------------------------------------


class _IdentifyDB:
    def __init__(self, profiles):
        self.profiles = profiles

    def fetchall(self, sql, *args):
        assert "speaker_profiles" in sql
        return self.profiles


async def test_identify_matches_enrolled_speaker(tmp_path):
    wav = tmp_path / "call.wav"
    _make_wav(wav, seed=9, seconds=3.0)
    # enroll: the profile embedding IS the stub embedding of this speaker's audio
    enrolled = StubEmbedder().embed(wav)
    db = _IdentifyDB([{"id": "p1", "name": "Alice", "embedding": enrolled}])
    # one speaker "S1" spanning the whole clip → concat == whole audio → matches
    turns = [{"start": 0.0, "end": 3.0, "speaker": "S1"}]
    out = _identify_speakers(db, wav, turns, "org-1", tmp_path, "j1")
    assert out == {"S1": "Alice"}


async def test_identify_no_profiles_is_empty(tmp_path):
    wav = tmp_path / "call.wav"
    _make_wav(wav)
    out = _identify_speakers(_IdentifyDB([]), wav, [{"start": 0, "end": 1, "speaker": "S1"}],
                             "org-1", tmp_path, "j1")
    assert out == {}
