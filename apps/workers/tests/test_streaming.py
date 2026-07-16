"""Tests for the streaming transcription service (gap #12).

The Whisper model is never loaded: a fake transcriber returns the window's
sample count as its text, so we can assert exactly which audio each final
covers. The e2e test drives the real FastAPI WebSocket app in-process via
Starlette's TestClient — real accept/receive/send frames, real state machine.
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


def _silence(seconds: float) -> bytes:
    """A PCM16 mono buffer of `seconds` of silence at SAMPLE_RATE."""
    return b"\x00\x00" * int(seconds * SAMPLE_RATE)


def _fake_transcriber(pcm: bytes, sample_rate: int) -> dict:
    """Deterministic: text is the window's sample count, so tests can assert
    exactly which range was transcribed."""
    return {"text": str(len(pcm) // BYTES_PER_SAMPLE)}


def test_session_finalizes_windows_in_order():
    cfg = StreamConfig(sample_rate=SAMPLE_RATE, window_seconds=3.0, partial_interval_seconds=1.0)
    sess = StreamSession(transcriber=_fake_transcriber, config=cfg)

    # Feed 7s in 1s chunks; windows of 3s → finals at 3s and 6s.
    finals: list[dict] = []
    partials: list[dict] = []
    for _ in range(7):
        for ev in sess.add_audio(_silence(1.0)):
            (finals if ev["type"] == "final" else partials).append(ev)

    assert [(f["start"], f["end"]) for f in finals] == [(0.0, 3.0), (3.0, 6.0)]
    # Each 3s final covers 48000 samples.
    assert [f["text"] for f in finals] == ["48000", "48000"]
    # Partials were emitted for the in-progress tail.
    assert partials and all(p["type"] == "partial" for p in partials)

    # Finalize the remaining 1s tail → one more final, then done.
    tail = sess.finalize()
    assert tail[0]["type"] == "final"
    assert (tail[0]["start"], tail[0]["end"]) == (6.0, 7.0)
    assert tail[0]["text"] == "16000"
    assert tail[-1] == {"type": "done", "text": "48000 48000 16000"}


def test_session_short_stream_only_final_on_finalize():
    # Under one window: no finals until finalize, then exactly one.
    cfg = StreamConfig(sample_rate=SAMPLE_RATE, window_seconds=3.0)
    sess = StreamSession(transcriber=_fake_transcriber, config=cfg)
    events = sess.add_audio(_silence(1.5))
    assert all(e["type"] == "partial" for e in events)  # no finals yet

    fin = sess.finalize()
    finals = [e for e in fin if e["type"] == "final"]
    assert len(finals) == 1
    assert (finals[0]["start"], finals[0]["end"]) == (0.0, 1.5)
    assert fin[-1]["type"] == "done"
    assert fin[-1]["text"] == "24000"  # 1.5s * 16000


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

        # Stream 7s of audio in 1s binary frames. Send all audio first; the
        # server queues its events in order for us to drain below.
        for _ in range(7):
            ws.send_bytes(_silence(1.0))
        ws.send_json({"type": "finalize"})

        # Drain every event up to and including `done`.
        events: list[dict] = []
        while True:
            ev = ws.receive_json()
            events.append(ev)
            if ev["type"] == "done":
                break

        finals = [e for e in events if e["type"] == "final"]
        partials = [e for e in events if e["type"] == "partial"]
        done = events[-1]

        # Three finals covering 0-3, 3-6, 6-7; done concatenates them.
        assert [(f["start"], f["end"]) for f in finals] == [(0.0, 3.0), (3.0, 6.0), (6.0, 7.0)]
        assert done == {"type": "done", "text": "48000 48000 16000"}
        # Interim partials were streamed (server-side; may be batched).
        assert all(p["type"] == "partial" for p in partials)


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
    # Guard: every event the session emits must be json.dumps-able (the WS
    # handler sends them with json.dumps).
    sess = StreamSession(transcriber=_fake_transcriber, config=StreamConfig())
    for ev in sess.add_audio(_silence(4.0)) + sess.finalize():
        json.dumps(ev)
