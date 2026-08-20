"""Tests for telephony ingestion (PRD 15 #355): μ-law codec + Twilio protocol."""

from __future__ import annotations

import base64

import numpy as np

from orpheus_workers.telephony import (
    parse_twilio_frame,
    pcm16_any_to_twilio_media,
    pcm16_to_ulaw,
    resample_linear,
    twilio_clear,
    twilio_media_to_pcm16_16k,
    ulaw_to_pcm16,
)

SR = 16000


def test_ulaw_roundtrip_preserves_signal():
    # a 200 Hz tone at 8 kHz → μ-law → back; μ-law is lossy but should track well
    t = np.arange(8000) / 8000
    pcm = (np.sin(2 * np.pi * 200 * t) * 20000).astype(np.int16)
    ulaw = pcm16_to_ulaw(pcm)
    assert len(ulaw) == len(pcm)
    back = ulaw_to_pcm16(ulaw)
    # correlation between original and round-tripped is very high
    corr = np.corrcoef(pcm.astype(np.float64), back.astype(np.float64))[0, 1]
    assert corr > 0.99


def test_ulaw_decode_known_values():
    # 0xFF is positive-zero, 0x7F negative-zero (both ~silence); 0x80/0x00 are the
    # positive/negative maxima.
    assert abs(int(ulaw_to_pcm16(bytes([0xFF]))[0])) < 20
    assert abs(int(ulaw_to_pcm16(bytes([0x7F]))[0])) < 20
    assert ulaw_to_pcm16(bytes([0x80]))[0] > 30000  # positive full-scale
    assert ulaw_to_pcm16(bytes([0x00]))[0] < -30000  # negative full-scale


def test_resample_changes_length_proportionally():
    x = np.ones(8000, dtype=np.int16) * 1000
    up = resample_linear(x, 8000, 16000)
    assert abs(len(up) - 16000) <= 1
    down = resample_linear(x, 8000, 8000)
    assert len(down) == 8000  # no-op when rates match


def test_twilio_inbound_to_pcm16_16k():
    # 20ms of μ-law 8k (160 samples) → PCM16 16k (~320 samples = 640 bytes)
    ulaw = pcm16_to_ulaw(np.zeros(160, dtype=np.int16))
    payload = base64.b64encode(ulaw).decode()
    pcm16 = twilio_media_to_pcm16_16k(payload)
    assert abs(len(pcm16) // 2 - 320) <= 2


def test_twilio_outbound_frames_are_20ms_mulaw():
    # 1s of PCM16 @ 24k → Twilio media frames (μ-law 8k, 160-sample chunks)
    pcm = np.zeros(24000, dtype=np.int16).tobytes()
    frames = pcm16_any_to_twilio_media(pcm, 24000, "SID123")
    assert frames and all(f["event"] == "media" and f["streamSid"] == "SID123" for f in frames)
    # ~1s @ 8k = 8000 samples / 160 = 50 frames
    assert 48 <= len(frames) <= 52
    # each payload decodes to <=160 μ-law bytes
    assert all(len(base64.b64decode(f["media"]["payload"])) <= 160 for f in frames)


def test_twilio_route_e2e_bridges_to_s2s():
    """Drive the Twilio route with synthetic Media Streams frames → the bot's TTS
    comes back as μ-law media frames, all transcoding handled in the route."""
    from fastapi.testclient import TestClient

    from orpheus_workers.streaming import create_app

    def complete_tx(pcm, sr):
        ws = [{"word": w, "start": i * 0.5, "end": (i + 1) * 0.5, "confidence": 0.9}
              for i, w in enumerate(["hello", "there", "friend."])]
        return {"words": ws, "text": "hello there friend."}

    app = create_app(transcriber=complete_tx, partial_transcriber=complete_tx)
    client = TestClient(app)
    # 20ms μ-law speech frame (loud) and a silence frame
    speech = base64.b64encode(pcm16_to_ulaw((np.ones(160) * 8000).astype(np.int16))).decode()
    silence = base64.b64encode(pcm16_to_ulaw(np.zeros(160, dtype=np.int16))).decode()

    with client.websocket_connect("/v1/telephony/twilio") as ws:
        ws.send_json({"event": "start", "start": {"streamSid": "SID1"}})
        # ~2s of speech then ~1s silence → a user turn ends → bot replies (stub TTS)
        for _ in range(100):
            ws.send_json({"event": "media", "media": {"payload": speech}})
        for _ in range(50):
            ws.send_json({"event": "media", "media": {"payload": silence}})
        ws.send_json({"event": "dtmf", "dtmf": {"digit": "5"}})
        ws.send_json({"event": "stop"})
        frames = []
        try:
            while True:
                frames.append(ws.receive_json())
        except Exception:
            pass

    events = {f.get("event") for f in frames}
    # DTMF was surfaced, and the bot's TTS came back as μ-law media frames
    assert "dtmf" in events
    assert any(f.get("event") == "media" and f.get("streamSid") == "SID1" for f in frames)


def test_twilio_clear_and_parse():
    assert twilio_clear("S1") == {"event": "clear", "streamSid": "S1"}
    assert parse_twilio_frame({"event": "start", "start": {"streamSid": "S9"}}) == {
        "kind": "start", "stream_sid": "S9"}
    assert parse_twilio_frame({"event": "media", "media": {"payload": "abc"}}) == {
        "kind": "media", "payload": "abc"}
    assert parse_twilio_frame({"event": "dtmf", "dtmf": {"digit": "5"}}) == {
        "kind": "dtmf", "digit": "5"}
    assert parse_twilio_frame({"event": "stop"}) == {"kind": "stop"}
