"""Tests for VAD chunking + utterance segmentation (PRD 11 §4.3, §4.4)."""

from __future__ import annotations

import wave

import numpy as np

from orpheus_workers.processors.transcribe import (
    _maybe_align,
    _transcribe_per_channel,
    _vad_chunk_boundaries,
    _vad_speech_regions,
)

SR = 16000


def _make_wav(tmp_path, pattern: list[tuple[str, float]]):
    parts = []
    for kind, sec in pattern:
        n = int(sec * SR)
        if kind == "speech":
            rng = np.random.default_rng(0)
            parts.append((rng.standard_normal(n) * 8000).astype(np.int16))
        else:
            parts.append(np.zeros(n, dtype=np.int16))
    audio = np.concatenate(parts)
    p = tmp_path / "a.wav"
    with wave.open(str(p), "wb") as w:
        w.setnchannels(1)
        w.setsampwidth(2)
        w.setframerate(SR)
        w.writeframes(audio.tobytes())
    return p


def test_vad_chunk_boundaries_cut_at_silence(tmp_path):
    # three 25s speech blocks split by 1s silence, max chunk 30s → cut at silences.
    p = _make_wav(
        tmp_path, [("speech", 25), ("silence", 1), ("speech", 25), ("silence", 1), ("speech", 25)]
    )
    b = _vad_chunk_boundaries(p, 30.0)
    assert all((e - s) <= 30.6 for s, e in b)  # respects the max
    cuts = [e for s, e in b][:-1]
    # cuts land near the silences at ~26s and ~52s, not mid-speech
    assert all(any(abs(c - x) < 1.5 for x in (26.0, 52.0)) for c in cuts)


def test_vad_chunk_boundaries_short_file_single_chunk(tmp_path):
    p = _make_wav(tmp_path, [("speech", 10)])
    assert _vad_chunk_boundaries(p, 30.0) == [(0.0, 10.0)]


def test_vad_speech_regions_splits_every_pause(tmp_path):
    # utterance segmentation cuts at EVERY pause, even in a short file.
    p = _make_wav(
        tmp_path, [("speech", 3), ("silence", 1), ("speech", 4), ("silence", 1), ("speech", 3)]
    )
    regions = _vad_speech_regions(p)
    assert len(regions) == 3
    # regions roughly match the three speech blocks (0-3, 4-8, 9-12)
    assert regions[0][0] < 0.5 and 2.5 < regions[0][1] < 3.5
    assert 3.5 < regions[1][0] < 4.5 and 7.5 < regions[1][1] < 8.5
    assert 8.5 < regions[2][0] < 9.5


def test_vad_speech_regions_no_pause_single_region(tmp_path):
    p = _make_wav(tmp_path, [("speech", 6)])
    regions = _vad_speech_regions(p)
    assert len(regions) == 1


def test_transcribe_per_channel_labels_and_separates(tmp_path, monkeypatch):
    import orpheus_workers.processors.transcribe as T

    def fake_extract(src, dst, ch):
        with wave.open(str(dst), "wb") as w:
            w.setnchannels(1)
            w.setsampwidth(2)
            w.setframerate(SR)
            w.writeframes(b"\x00\x00" * SR)

    calls = {"n": 0}

    def fake_full(wav, dur, cs, wt, topts, params, wd, jid):
        ch = calls["n"]
        calls["n"] += 1
        # channel 1's segment starts earlier in time → tests time-ordered merge
        start = 2.0 - ch
        return {
            "segments": [{"start": start, "end": start + 1.0, "text": f"speaker{ch}"}],
            "text": f"speaker{ch}",
            "language": "en",
        }

    monkeypatch.setattr(T, "extract_channel_16k_mono", fake_extract)
    monkeypatch.setattr(T, "_transcribe_wav_full", fake_full)

    res = _transcribe_per_channel(
        tmp_path / "src.wav",
        2,
        5.0,
        60,
        False,
        {"language": None},
        {"channel_labels": ["agent", "customer"]},
        str(tmp_path),
        "j1",
    )
    assert res["channels"] == [{"index": 0, "label": "agent"}, {"index": 1, "label": "customer"}]
    # every segment carries its channel; channels are fully separated (no bleed)
    ch_of = {s["text"]: s["channel"] for s in res["segments"]}
    assert ch_of == {"speaker0": 0, "speaker1": 1}
    # merged transcript is time-ordered: channel 1 (start 1.0) before channel 0 (start 2.0)
    assert [s["channel"] for s in res["segments"]] == [1, 0]
    assert res["text"] == "speaker1 speaker0"


def test_maybe_align_noop_when_not_forced(tmp_path):
    p = _make_wav(tmp_path, [("speech", 1)])
    tr = {"segments": [{"start": 0.0, "end": 1.0, "text": "hi", "words": []}]}
    _maybe_align(tr, p, {"alignment": "off"}, "job1")
    assert "alignment" not in tr  # untouched


def test_maybe_align_degrades_gracefully_on_failure(tmp_path, monkeypatch):
    # alignment="forced" but modal backend is unconfigured → AlignError, which
    # _maybe_align must swallow (keep whisper timings, job still succeeds).
    monkeypatch.setenv("ORPHEUS_ALIGN_BACKEND", "modal")
    monkeypatch.delenv("ORPHEUS_MODAL_ALIGN_URL", raising=False)
    monkeypatch.delenv("ORPHEUS_MODAL_ALIGN_TOKEN", raising=False)
    p = _make_wav(tmp_path, [("speech", 1)])
    words = [{"word": "hi", "start": 0.0, "end": 1.0, "confidence": 0.5}]
    tr = {"segments": [{"start": 0.0, "end": 1.0, "text": "hi", "words": words}]}
    _maybe_align(tr, p, {"alignment": "forced"}, "job1")  # must not raise
    assert tr["segments"][0]["words"] == words  # original timings preserved
