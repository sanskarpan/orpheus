"""Tests for the streaming transcription service (gap #12).

The Whisper model is never loaded: a fake transcriber turns the buffer length
into a deterministic word sequence (one word per 0.5 s), so we can assert the
LocalAgreement-2 confirmation, buffer trimming, and VAD endpointing exactly.
The e2e test drives the real FastAPI WebSocket app in-process.
"""

from __future__ import annotations

import json

from orpheus_workers.streaming import (
    StreamConfig,
    StreamSession,
    create_app,
    pcm16_to_wav_file,
)

SAMPLE_RATE = 16_000
BYTES_PER_SAMPLE = 2
WORD_SECONDS = 0.5


def _tone(seconds: float) -> bytes:
    """Non-silent PCM16 (constant amplitude) so VAD sees it as speech."""
    return b"\x00\x40" * int(seconds * SAMPLE_RATE)


def _silence(seconds: float) -> bytes:
    return b"\x00\x00" * int(seconds * SAMPLE_RATE)


def _fake_transcriber(pcm: bytes, sample_rate: int) -> dict:
    """Deterministic: one word ``wN`` per 0.5 s of buffer, with timestamps.

    Re-decoding a longer buffer reproduces the same prefix, so LocalAgreement
    confirms the stable prefix — exactly what the real model does when the tail
    settles.
    """
    seconds = (len(pcm) // BYTES_PER_SAMPLE) / sample_rate
    n = int(round(seconds / WORD_SECONDS))
    words = [
        {"word": f"w{i}", "start": i * WORD_SECONDS, "end": (i + 1) * WORD_SECONDS}
        for i in range(n)
    ]
    return {"words": words, "text": " ".join(w["word"] for w in words)}


def _no_vad(**kw) -> StreamConfig:
    return StreamConfig(sample_rate=SAMPLE_RATE, vad_enabled=False, **kw)


def test_localagreement_confirms_stable_prefix():
    sess = StreamSession(transcriber=_fake_transcriber, config=_no_vad())
    finals: list[dict] = []
    partials: list[dict] = []
    # Feed 5 s in 1 s tones; each decode agrees with the previous prefix.
    for _ in range(5):
        for ev in sess.add_audio(_tone(1.0)):
            (finals if ev["type"] == "final" else partials).append(ev)

    # Confirmed words are contiguous, in order, non-overlapping.
    text = " ".join(f["text"] for f in finals).split()
    assert text == [f"w{i}" for i in range(len(text))]
    assert len(text) >= 6  # most of the 10 words confirmed before the last tail
    assert partials and all(p["type"] == "partial" for p in partials)

    # Finalize confirms the rest; done has the full transcript in order.
    done = sess.finalize()[-1]
    assert done["type"] == "done"
    assert done["text"].split() == [f"w{i}" for i in range(10)]


def test_finalize_flushes_short_stream():
    sess = StreamSession(transcriber=_fake_transcriber, config=_no_vad())
    # 1.5 s: below the agreement cadence — nothing confirmed until finalize.
    sess.add_audio(_tone(1.5))
    fin = sess.finalize()
    finals = [e for e in fin if e["type"] == "final"]
    assert len(finals) == 1
    assert finals[0]["text"].split() == ["w0", "w1", "w2"]  # 1.5s / 0.5
    assert fin[-1]["type"] == "done" and fin[-1]["text"] == "w0 w1 w2"


def test_buffer_is_trimmed_to_bound_cost():
    # Small max_buffer forces a trim once words are confirmed; the absolute
    # offset advances and the buffer shrinks, so re-transcription stays bounded.
    sess = StreamSession(transcriber=_fake_transcriber, config=_no_vad(max_buffer_seconds=3.0))
    finals: list[dict] = []
    for _ in range(6):
        finals += [e for e in sess.add_audio(_tone(1.0)) if e["type"] == "final"]
    assert sess._offset_s > 0.0  # a trim happened
    # The buffer never grew past the bound → re-transcription cost stays bounded.
    assert sess._buffer_samples < sess.config.max_buffer_samples
    # Finals stream out (in absolute time order) rather than piling up in one buffer.
    assert finals and all(
        finals[i]["start"] <= finals[i + 1]["start"] for i in range(len(finals) - 1)
    )
    done = sess.finalize()[-1]
    assert done["type"] == "done" and done["text"]


def test_vad_endpoints_on_trailing_silence():
    sess = StreamSession(
        transcriber=_fake_transcriber,
        config=StreamConfig(sample_rate=SAMPLE_RATE, vad_silence_seconds=0.5, vad_energy_ratio=0.3),
    )
    sess.add_audio(_tone(2.0))  # speech
    events = sess.add_audio(_silence(1.0))  # a pause → endpoint + flush
    finals = [e for e in events if e["type"] == "final"]
    assert finals, "trailing silence should flush a final"
    assert sess._offset_s > 0.0  # endpoint trimmed the buffer


def test_semantic_endpoint_holds_midsentence_pause():
    from orpheus_workers.streaming import SemanticEndpointDetector

    cfg = StreamConfig(vad_silence_seconds=0.5, turn_max_silence_seconds=1.5)
    det = SemanticEndpointDetector(cfg)
    # A short pause + dangling word ("and") → NOT a turn end; only a long pause is.
    probes = {0.5: True, 1.5: False}
    assert det.is_endpoint("i went to the store and", lambda s: probes[s]) is False
    # Same dangling text but the pause is now long → endpoint.
    probes_long = {0.5: True, 1.5: True}
    assert det.is_endpoint("i went to the store and", lambda s: probes_long[s]) is True
    # A complete sentence + a short pause → endpoint immediately.
    assert det.is_endpoint("that is all for today.", lambda s: probes[s]) is True
    # Still speaking (no short-pause) → never an endpoint.
    assert det.is_endpoint("that is all for today.", lambda s: False) is False


def test_energy_endpoint_is_content_blind():
    from orpheus_workers.streaming import EnergyEndpointDetector

    cfg = StreamConfig(vad_silence_seconds=0.5)
    det = EnergyEndpointDetector(cfg)
    # Only the short-silence probe matters; text is ignored.
    assert det.is_endpoint("i went to the store and", lambda s: s == 0.5) is True
    assert det.is_endpoint("that is all for today.", lambda s: False) is False


def test_modal_endpoint_falls_back_to_semantic_when_unset(monkeypatch):
    from orpheus_workers.streaming import ModalEndpointDetector

    monkeypatch.delenv("ORPHEUS_MODAL_TURN_URL", raising=False)
    cfg = StreamConfig(vad_silence_seconds=0.5, turn_max_silence_seconds=1.5)
    det = ModalEndpointDetector(cfg)
    # No turn URL → behaves exactly like the semantic detector.
    assert det.is_endpoint("that is all for today.", lambda s: s == 0.5) is True
    assert det.is_endpoint("i went to the store and", lambda s: s == 0.5) is False


def test_semantic_backend_suppresses_endpoint_in_session():
    # A transcriber whose hypothesis ends on a dangling word ("and"); the buffer
    # "reads" mid-sentence. Energy would endpoint on any pause; semantic holds.
    def midsentence_tx(pcm, sr):
        ws = [
            {"word": w, "start": i * 0.5, "end": (i + 1) * 0.5, "confidence": 0.9}
            for i, w in enumerate(["i", "went", "to", "the", "store", "and"])
        ]
        return {"words": ws, "text": " ".join(w["word"] for w in ws)}

    def run(backend):
        sess = StreamSession(
            transcriber=midsentence_tx,
            config=StreamConfig(
                sample_rate=SAMPLE_RATE,
                turn_backend=backend,
                vad_silence_seconds=0.5,
                turn_max_silence_seconds=1.5,
            ),
        )
        # First decode (prev empty → agreement confirms nothing), so ONLY an
        # endpoint can flush a final: 2 s speech + a 0.6 s pause.
        return sess.add_audio(_tone(2.0) + _silence(0.6))

    energy_finals = [e for e in run("energy") if e["type"] == "final"]
    semantic_finals = [e for e in run("semantic") if e["type"] == "final"]
    assert energy_finals, "energy endpoints on the pause and flushes a final"
    assert not semantic_finals, "semantic holds the mid-sentence pause (no endpoint)"


def test_turn_backend_selected_from_config():
    from orpheus_workers.streaming import (
        EnergyEndpointDetector,
        SemanticEndpointDetector,
        make_endpoint_detector,
    )

    assert isinstance(make_endpoint_detector(StreamConfig()), EnergyEndpointDetector)
    assert isinstance(
        make_endpoint_detector(StreamConfig(turn_backend="semantic")),
        SemanticEndpointDetector,
    )


def _complete_tx(pcm, sr):
    """Hypothesis that always reads complete ('... today.'), for eager-turn tests."""
    ws = [
        {"word": w, "start": i * 0.5, "end": (i + 1) * 0.5, "confidence": 0.9}
        for i, w in enumerate(["all", "done", "today."])
    ]
    return {"words": ws, "text": "all done today."}


def test_eager_speculative_then_firm_endpoint_advances_turn():
    sess = StreamSession(
        transcriber=_complete_tx,
        config=StreamConfig(
            sample_rate=SAMPLE_RATE,
            vad_silence_seconds=0.6,
            eager_endpoint_seconds=0.25,
        ),
    )
    # 2 s speech + a 0.3 s pause: past eager (0.25) but under firm (0.6) → speculate.
    ev1 = sess.add_audio(_tone(2.0) + _silence(0.3))
    spec = [e for e in ev1 if e["type"] == "endpoint_speculative"]
    assert spec and spec[0]["turn_id"] == 0
    assert not [e for e in ev1 if e["type"] == "endpoint"]  # not firm yet

    # Extend the pause past the firm threshold (and the commit cadence) → firm
    # endpoint, turn advances.
    ev2 = sess.add_audio(_silence(1.0))
    firm = [e for e in ev2 if e["type"] == "endpoint"]
    assert firm and firm[0]["turn_id"] == 0
    assert sess._turn_id == 1 and sess._speculative is False


def test_eager_endpoint_cancels_when_speech_resumes():
    # First a completed utterance + short pause (speculate), then more speech
    # resumes before the firm endpoint → cancel.
    state = {"mode": "complete"}

    def tx(pcm, sr):
        if state["mode"] == "complete":
            return _complete_tx(pcm, sr)
        ws = [  # speaker continued → longer, still-incomplete hypothesis
            {"word": w, "start": i * 0.5, "end": (i + 1) * 0.5, "confidence": 0.9}
            for i, w in enumerate(["all", "done", "today", "and", "also"])
        ]
        return {"words": ws, "text": "all done today and also"}

    sess = StreamSession(
        transcriber=tx,
        config=StreamConfig(
            sample_rate=SAMPLE_RATE, vad_silence_seconds=0.6, eager_endpoint_seconds=0.25
        ),
    )
    ev1 = sess.add_audio(_tone(2.0) + _silence(0.3))
    assert [e for e in ev1 if e["type"] == "endpoint_speculative"]
    assert sess._speculative is True
    # Speech resumes (no trailing silence, incomplete hypothesis) → cancel.
    state["mode"] = "resumed"
    ev2 = sess.add_audio(_tone(1.0))
    cancels = [e for e in ev2 if e["type"] == "endpoint_cancel"]
    assert cancels and cancels[0]["turn_id"] == 0
    assert sess._speculative is False
    assert not [e for e in ev2 if e["type"] == "endpoint"]  # no firm end happened


def test_instream_diarization_labels_speakers_and_emits_updates():
    # Two distinct voices as orthogonal embeddings, chosen by the amplitude of the
    # window's TAIL (the current speaker) — matching the trailing-window embed,
    # which extends back past the just-confirmed group for stable geometry.
    def embedder(pcm, sr):
        import numpy as np

        buf = np.frombuffer(pcm, dtype=np.int16).astype(np.float32)
        tail = buf[-int(0.3 * sr) :] if buf.size >= int(0.3 * sr) else buf
        loud = float(np.sqrt(np.mean(tail**2))) if tail.size else 0.0
        return [1.0, 0.0] if loud > 6000 else [0.0, 1.0]

    def loud(sec):  # "speaker A"
        return b"\x00\x40" * int(sec * SAMPLE_RATE)  # amp 16384

    def soft(sec):  # "speaker B"
        return b"\x00\x08" * int(sec * SAMPLE_RATE)  # amp 2048 (non-silent)

    words = ["one", "two", "three", "four", "five", "six"]
    sess = StreamSession(
        transcriber=_words_transcriber(words),
        embedder=embedder,
        config=StreamConfig(
            sample_rate=SAMPLE_RATE, vad_enabled=False, diarize=True, diarize_min_seconds=0.3
        ),
    )
    events: list[dict] = []
    events += sess.add_audio(loud(2.0))  # speaker A
    events += sess.add_audio(soft(2.0))  # speaker B
    events += sess.add_audio(loud(2.0))  # speaker A again
    events += sess.finalize()

    finals = [e for e in events if e["type"] == "final"]
    updates = [e for e in events if e["type"] == "speaker_update"]
    assert finals and all("speaker" in f for f in finals)
    labels = [f["speaker"] for f in finals]
    assert set(labels) == {"S1", "S2"}  # exactly two speakers discovered
    assert "S1" in labels and "S2" in labels
    # A speaker_update is emitted each time the active speaker changes.
    assert [u["speaker"] for u in updates][:2] == ["S1", "S2"]


def test_diarization_off_by_default_no_speaker_field():
    sess = StreamSession(
        transcriber=_words_transcriber(["a", "b"]),
        embedder=lambda pcm, sr: [1.0, 0.0],
        config=_no_vad(),  # diarize defaults False
    )
    events = sess.add_audio(_tone(1.0)) + sess.finalize()
    finals = [e for e in events if e["type"] == "final"]
    assert finals and not any("speaker" in f for f in finals)
    assert not [e for e in events if e["type"] == "speaker_update"]


def test_diarization_degrades_when_embedder_fails():
    def broken(pcm, sr):
        raise RuntimeError("embed model down")

    sess = StreamSession(
        transcriber=_words_transcriber(["a", "b", "c"]),
        embedder=broken,
        config=StreamConfig(sample_rate=SAMPLE_RATE, vad_enabled=False, diarize=True),
    )
    # Must not raise; finals still stream (speaker falls back to active/None).
    events = sess.add_audio(_tone(2.0)) + sess.finalize()
    assert [e for e in events if e["type"] == "final"]


def _turn_cfg(**kw):
    return StreamConfig(sample_rate=SAMPLE_RATE, turn_events=True, vad_silence_seconds=0.5, **kw)


def test_turn_events_speech_start_and_end():
    sess = StreamSession(transcriber=_fake_transcriber, config=_turn_cfg())
    ev1 = sess.add_audio(_tone(1.0))  # speech onset
    assert any(e["type"] == "speech_start" for e in ev1)
    ev2 = sess.add_audio(_silence(1.0))  # sustained pause → speech_end
    assert any(e["type"] == "speech_end" for e in ev2)


def test_turn_events_barge_in_when_bot_speaking():
    sess = StreamSession(transcriber=_fake_transcriber, config=_turn_cfg(bot_speaking=True))
    ev = sess.add_audio(_tone(1.0))
    types = [e["type"] for e in ev]
    assert "speech_start" in types and "barge_in" in types
    # no barge_in when the bot isn't speaking
    sess2 = StreamSession(transcriber=_fake_transcriber, config=_turn_cfg(bot_speaking=False))
    assert "barge_in" not in [e["type"] for e in sess2.add_audio(_tone(1.0))]


def test_backchannel_detected():
    sess = StreamSession(
        transcriber=_words_transcriber(["yeah"]), config=_turn_cfg(vad_enabled=False)
    )
    sess.add_audio(_tone(0.5))
    events = sess.finalize()
    assert any(e["type"] == "backchannel" and e["text"] == "yeah" for e in events)


def test_non_backchannel_not_tagged():
    sess = StreamSession(
        transcriber=_words_transcriber(["please", "transfer", "me", "now"]),
        config=_turn_cfg(vad_enabled=False),
    )
    for _ in range(2):
        sess.add_audio(_tone(1.0))
    events = sess.finalize()
    assert not any(e["type"] == "backchannel" for e in events)


def test_voicemail_detected_once():
    words = ["you've", "reached", "the", "voicemail", "please", "leave", "a", "message"]
    sess = StreamSession(transcriber=_words_transcriber(words), config=_turn_cfg(vad_enabled=False))
    events = []
    for _ in range(4):
        events += sess.add_audio(_tone(1.0))
    events += sess.finalize()
    vm = [e for e in events if e["type"] == "voicemail"]
    assert len(vm) == 1  # emitted exactly once


def test_turn_events_off_by_default():
    sess = StreamSession(transcriber=_fake_transcriber, config=_no_vad())
    ev = sess.add_audio(_tone(1.0))
    assert not any(e["type"] in ("speech_start", "barge_in") for e in ev)


def test_shrinking_hypothesis_does_not_crash():
    # A re-decode returning FEWER words than were committed must not IndexError
    # (real models revise words near the buffer edge). Regression for a HIGH bug.
    state = {"n": 0}

    def tx(pcm, sr):
        state["n"] += 1
        seq = ["a", "b", "c", "d"] if state["n"] <= 2 else ["a", "b"]
        return {
            "words": [
                {"word": w, "start": i * 0.5, "end": (i + 1) * 0.5} for i, w in enumerate(seq)
            ],
            "text": " ".join(seq),
        }

    sess = StreamSession(transcriber=tx, config=_no_vad())
    sess.add_audio(_tone(1.0))
    sess.add_audio(_tone(1.0))  # confirms [a,b,c,d] → _committed=4
    # finalize now sees only 2 words → _committed must clamp, no crash.
    done = sess.finalize()[-1]
    assert done["type"] == "done"


def test_buffer_bounded_when_hypothesis_never_agrees():
    # Continuous speech + an unstable hypothesis (never agrees) must NOT grow the
    # buffer without bound — the max_buffer backstop must fire even at _committed=0.
    state = {"n": 0}

    def tx(pcm, sr):
        state["n"] += 1
        return {
            "words": [{"word": f"w{state['n']}", "start": 0.0, "end": 0.5}],
            "text": f"w{state['n']}",
        }

    sess = StreamSession(transcriber=tx, config=_no_vad(max_buffer_seconds=3.0))
    for _ in range(12):  # 12s of audio, 3s cap
        sess.add_audio(_tone(1.0))
    assert sess._buffer_samples <= sess.config.max_buffer_samples  # bounded at the cap
    assert sess._offset_s > 0.0  # audio was actually dropped/trimmed


def test_commit_transcriber_exception_does_not_crash():
    # A transient commit-model error must be swallowed, not kill the session.
    def boom(pcm, sr):
        raise RuntimeError("model down")

    sess = StreamSession(transcriber=boom, config=_no_vad())
    assert sess.add_audio(_tone(1.0)) == []  # no crash, no events
    assert sess.finalize()[-1]["type"] == "done"


def test_pcm16_to_wav_roundtrip(tmp_path):
    import wave

    pcm = _silence(0.5)
    out = tmp_path / "s.wav"
    pcm16_to_wav_file(pcm, SAMPLE_RATE, out)
    with wave.open(str(out), "rb") as w:
        assert w.getnchannels() == 1
        assert w.getframerate() == SAMPLE_RATE
        assert w.getnframes() == int(0.5 * SAMPLE_RATE)


def test_websocket_e2e_streams_partials_and_finals():
    from fastapi.testclient import TestClient

    app = create_app(transcriber=_fake_transcriber)
    client = TestClient(app)

    with client.websocket_connect("/v1/stream/transcribe") as ws:
        ws.send_json({"type": "start", "sample_rate": SAMPLE_RATE})
        assert ws.receive_json() == {"type": "ready"}

        for _ in range(5):
            ws.send_bytes(_tone(1.0))
        ws.send_json({"type": "finalize"})

        events: list[dict] = []
        while True:
            ev = ws.receive_json()
            events.append(ev)
            if ev["type"] == "done":
                break

        finals = [e for e in events if e["type"] == "final"]
        partials = [e for e in events if e["type"] == "partial"]
        done = events[-1]
        assert finals and all(f["type"] == "final" for f in finals)
        assert all(p["type"] == "partial" for p in partials)
        assert done["text"].split() == [f"w{i}" for i in range(10)]


def test_websocket_rejects_bad_control():
    from fastapi.testclient import TestClient

    app = create_app(transcriber=_fake_transcriber)
    client = TestClient(app)
    with client.websocket_connect("/v1/stream/transcribe") as ws:
        ws.send_text("not json{")
        assert ws.receive_json() == {"type": "error", "error": "invalid json"}
        ws.send_json({"type": "bogus"})
        err = ws.receive_json()
        assert err["type"] == "error" and "bogus" in err["error"]


def test_json_events_are_serializable():
    sess = StreamSession(transcriber=_fake_transcriber, config=_no_vad())
    evs = sess.add_audio(_tone(4.0)) + sess.finalize()
    for ev in evs:
        json.dumps(ev)


def _words_transcriber(word_list):
    """Transcriber that reveals `word_list` as the buffer grows (one word / 0.5s)."""

    def tx(pcm, sr):
        secs = (len(pcm) // BYTES_PER_SAMPLE) / sr
        n = int(round(secs / WORD_SECONDS))
        ws = [
            {"word": w, "start": i * WORD_SECONDS, "end": (i + 1) * WORD_SECONDS, "confidence": 0.9}
            for i, w in enumerate(word_list[:n])
        ]
        return {"words": ws, "text": " ".join(w["word"] for w in ws)}

    return tx


def test_final_words_carry_confidence():
    sess = StreamSession(transcriber=_words_transcriber(["hello", "world"]), config=_no_vad())
    sess.add_audio(_tone(1.0))
    fin = sess.finalize()
    finals = [e for e in fin if e["type"] == "final"]
    assert finals and all("words" in f for f in finals)
    allw = [w for f in finals for w in f["words"]]
    assert allw and all("confidence" in w and 0.0 <= w["confidence"] <= 1.0 for w in allw)


def test_realtime_redaction_masks_pii_in_finals():
    sess = StreamSession(
        transcriber=_words_transcriber(["email", "me", "at", "alice@example.com", "today"]),
        config=_no_vad(),
        redact={"enabled": True, "entities": ["EMAIL"], "mask": "type"},
    )
    events: list[dict] = []
    for _ in range(5):
        events += sess.add_audio(_tone(1.0))
    events += sess.finalize()
    finals = [e for e in events if e["type"] == "final"]
    done = events[-1]
    text = " ".join(f["text"] for f in finals)
    assert "alice@example.com" not in text and "[EMAIL]" in text
    # words[] must not leak the raw PII token either
    allw = [w["word"] for f in finals for w in f["words"]]
    assert "alice@example.com" not in allw and "[EMAIL]" in allw
    assert done.get("redactions") == [{"entity": "EMAIL", "count": 1}]


def _labeled_transcriber(prefix: str):
    """One word ``{prefix}{i}`` per 0.5 s of buffer — to tell the fast partial
    model's output apart from the commit model's."""

    def tx(pcm: bytes, sr: int) -> dict:
        secs = (len(pcm) // BYTES_PER_SAMPLE) / sr
        n = int(round(secs / WORD_SECONDS))
        ws = [
            {
                "word": f"{prefix}{i}",
                "start": i * WORD_SECONDS,
                "end": (i + 1) * WORD_SECONDS,
                "confidence": 0.8,
            }
            for i in range(n)
        ]
        return {"words": ws, "text": " ".join(w["word"] for w in ws)}

    return tx


def test_fast_partial_fires_before_commit_cadence():
    # 0.3 s is below the 1.0 s commit cadence but above the 0.25 s partial cadence,
    # so a provisional arrives from the fast model with no final yet.
    sess = StreamSession(
        transcriber=_labeled_transcriber("c"),
        partial_transcriber=_labeled_transcriber("p"),
        config=_no_vad(min_chunk_seconds=1.0, partial_chunk_seconds=0.25),
    )
    events = sess.add_audio(_tone(0.3))
    partials = [e for e in events if e["type"] == "partial"]
    assert partials, "fast partial should fire before the first commit"
    assert partials[0]["text"].startswith("p")  # came from the fast partial model
    assert not [e for e in events if e["type"] == "final"]  # nothing committed yet


def test_fast_partial_decodes_only_uncommitted_tail():
    sess = StreamSession(
        transcriber=_labeled_transcriber("c"),
        partial_transcriber=_labeled_transcriber("p"),
        config=_no_vad(min_chunk_seconds=1.0, partial_chunk_seconds=0.25),
    )
    # Two commit passes confirm the stable prefix (LocalAgreement needs 2 hyps).
    sess.add_audio(_tone(1.0))
    sess.add_audio(_tone(1.0))
    assert sess._committed_local_end_s > 0.0  # some words confirmed
    committed_end = sess._offset_s + sess._committed_local_end_s
    # A sub-commit feed → fast partial of only the tail past the confirmed words.
    events = sess.add_audio(_tone(0.3))
    partials = [e for e in events if e["type"] == "partial"]
    assert partials
    assert partials[0]["start"] >= committed_end - 1e-6  # base offset applied
    assert partials[0]["text"].startswith("p")


def test_partial_cadence_disabled_falls_back_to_commit_only():
    sess = StreamSession(
        transcriber=_labeled_transcriber("c"),
        partial_transcriber=_labeled_transcriber("p"),
        config=_no_vad(min_chunk_seconds=1.0, partial_chunk_seconds=0.0),
    )
    # Sub-commit feeds emit nothing when the fast loop is disabled.
    assert sess.add_audio(_tone(0.3)) == []
    assert sess.add_audio(_tone(0.3)) == []


def test_partials_marked_provisional_when_redacting():
    sess = StreamSession(
        transcriber=_words_transcriber(["w0", "w1", "w2", "w3"]),
        config=_no_vad(),
        redact={"enabled": True},
    )
    events = []
    for _ in range(3):
        events += sess.add_audio(_tone(1.0))
    partials = [e for e in events if e["type"] == "partial"]
    assert partials and all(p.get("redacted_provisional") is True for p in partials)
