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
    assert fin[-1] == {"type": "done", "text": "w0 w1 w2"}


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
