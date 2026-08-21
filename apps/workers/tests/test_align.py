"""Tests for forced alignment (PRD 11) — the worker seam + assembly.

The torch/torchaudio model is never loaded: the modal backend is exercised via a
monkeypatched HTTP call and the assembly is unit-tested directly.
"""

from __future__ import annotations

import pytest

from orpheus_workers import align as align_mod
from orpheus_workers.align import (
    AlignError,
    _apply_aligned_segments,
    _segment_tokens,
    align_transcript,
)


def test_segment_tokens_splits_words_keeps_apostrophes():
    assert _segment_tokens("It's a test, really.") == ["It's", "a", "test", "really"]
    assert _segment_tokens("") == []


def test_apply_aligned_segments_replaces_words_and_tightens_bounds():
    tr = {
        "text": "hello world",
        "segments": [{"start": 0.0, "end": 2.0, "text": "hello world", "words": []}],
    }
    aligned = [
        {
            "words": [
                {"word": "hello", "start": 0.12, "end": 0.44, "confidence": 0.9},
                {"word": "world", "start": 0.51, "end": 0.88, "confidence": 0.8},
            ]
        }
    ]
    _apply_aligned_segments(tr, aligned)
    seg = tr["segments"][0]
    assert [w["word"] for w in seg["words"]] == ["hello", "world"]
    assert seg["start"] == 0.12 and seg["end"] == 0.88  # tightened to aligned bounds
    assert tr["alignment"] == "forced"


def test_apply_aligned_keeps_segment_when_no_words():
    tr = {"segments": [{"start": 0.0, "end": 1.0, "text": "x", "words": [{"word": "x"}]}]}
    _apply_aligned_segments(tr, [{"words": []}])
    # empty alignment for a segment leaves its existing words untouched
    assert tr["segments"][0]["words"] == [{"word": "x"}]


def test_align_transcript_modal_backend(monkeypatch):
    monkeypatch.setenv("ORPHEUS_ALIGN_BACKEND", "modal")
    monkeypatch.setenv("ORPHEUS_MODAL_ALIGN_URL", "https://align.example/align")
    monkeypatch.setenv("ORPHEUS_MODAL_ALIGN_TOKEN", "secret")

    captured = {}

    class _Resp:
        def raise_for_status(self):
            pass

        def json(self):
            return {
                "segments": [
                    {"words": [{"word": "hi", "start": 0.2, "end": 0.5, "confidence": 0.95}]}
                ]
            }

    def fake_post(url, payload, timeout=600.0):
        captured["url"] = url
        captured["segments"] = payload["segments"]
        captured["token"] = payload["token"]
        return _Resp()

    monkeypatch.setattr("orpheus_workers.modal_client.modal_post_json", fake_post)

    tr = {"segments": [{"start": 0.0, "end": 1.0, "text": "hi", "words": []}]}
    # write a tiny dummy wav path (read as bytes for b64) — content is irrelevant here
    import wave

    wav = align_mod.Path("/tmp/_align_test.wav")
    with wave.open(str(wav), "wb") as w:
        w.setnchannels(1)
        w.setsampwidth(2)
        w.setframerate(16000)
        w.writeframes(b"\x00\x00" * 1600)

    align_transcript(wav, tr, language="en")
    assert captured["token"] == "secret"
    assert captured["segments"][0]["text"] == "hi"
    assert tr["segments"][0]["words"][0]["word"] == "hi"
    assert tr["segments"][0]["start"] == 0.2 and tr["segments"][0]["end"] == 0.5
    assert tr["alignment"] == "forced"


def test_align_transcript_modal_missing_config_raises(monkeypatch):
    monkeypatch.setenv("ORPHEUS_ALIGN_BACKEND", "modal")
    monkeypatch.delenv("ORPHEUS_MODAL_ALIGN_URL", raising=False)
    monkeypatch.delenv("ORPHEUS_MODAL_ALIGN_TOKEN", raising=False)
    tr = {"segments": [{"start": 0.0, "end": 1.0, "text": "hi", "words": []}]}
    with pytest.raises(AlignError):
        align_transcript("/tmp/whatever.wav", tr)


def test_align_transcript_no_segments_is_noop():
    tr = {"segments": []}
    assert align_transcript("/tmp/x.wav", tr) is tr


def test_align_transcript_unknown_backend_raises(monkeypatch):
    monkeypatch.setenv("ORPHEUS_ALIGN_BACKEND", "bogus")
    tr = {"segments": [{"start": 0.0, "end": 1.0, "text": "hi", "words": []}]}
    with pytest.raises(AlignError):
        align_transcript("/tmp/x.wav", tr)
