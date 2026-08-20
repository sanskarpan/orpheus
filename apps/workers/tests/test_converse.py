"""Tests for the speech-to-speech loop (PRD 15 #356): ConverseSession + route."""

from __future__ import annotations

from orpheus_workers.converse import ConverseSession
from orpheus_workers.streaming import StreamConfig, StreamSession

SR = 16000
BPS = 2


def _tone(seconds):
    return b"\x00\x40" * int(seconds * SR)


def _silence(seconds):
    return b"\x00\x00" * int(seconds * SR)


def _complete_tx(pcm, sr):
    ws = [
        {"word": w, "start": i * 0.5, "end": (i + 1) * 0.5, "confidence": 0.9}
        for i, w in enumerate(["hello", "there", "friend."])
    ]
    return {"words": ws, "text": "hello there friend."}


class FakeLLM:
    model_version_id = "fake-llm"

    def __init__(self):
        self.calls = []

    def complete(self, system, user, max_tokens=512):
        self.calls.append(user)
        return "Sure, I can help with that."


class FakeTTS:
    model_version_id = "fake-tts"

    def synth(self, text, voice=None):
        # a tiny valid wav
        import io
        import wave

        buf = io.BytesIO()
        with wave.open(buf, "wb") as w:
            w.setnchannels(1)
            w.setsampwidth(2)
            w.setframerate(24000)
            w.writeframes(b"\x00\x00" * 2400)
        return buf.getvalue(), 24000


def _session(llm=None, tts=None):
    asr = StreamSession(
        transcriber=_complete_tx,
        config=StreamConfig(sample_rate=SR, turn_events=True, vad_silence_seconds=0.5),
    )
    return ConverseSession(asr=asr, llm=llm or FakeLLM(), tts=tts or FakeTTS())


def test_converse_user_turn_produces_bot_reply_and_tts():
    sess = _session()
    events = []
    events += sess.add_audio(_tone(2.0))  # user speaks
    events += sess.add_audio(_silence(1.0))  # pause → endpoint → agent responds
    replies = [e for e in events if e["type"] == "bot_reply"]
    tts = [e for e in events if e["type"] == "tts_audio"]
    assert replies and replies[0]["text"] == "Sure, I can help with that."
    assert tts and tts[0]["sample_rate"] == 24000 and tts[0]["audio_b64"]
    # the LLM saw the user's transcribed turn
    assert "hello there friend." in sess.llm.calls[0]


def test_converse_bot_reply_turn_id_matches_user_turn():
    # The agent's reply must carry the SAME turn_id as the user turn it answers
    # (not user_turn+1). Regression for the turn-desync bug.
    sess = _session()
    events = sess.add_audio(_tone(2.0)) + sess.add_audio(_silence(1.0))
    endpoint = [e for e in events if e["type"] == "endpoint"]
    reply = [e for e in events if e["type"] == "bot_reply"]
    assert endpoint and reply
    assert reply[0]["turn_id"] == endpoint[0]["turn_id"]


def test_converse_barge_in_emits_tts_cancel():
    sess = _session()
    sess.asr.config.bot_speaking = True
    events = sess.add_audio(_tone(1.0))  # user talks while bot "speaking"
    types = [e["type"] for e in events]
    assert "barge_in" in types and "tts_cancel" in types


def test_converse_llm_failure_degrades():
    class BrokenLLM:
        model_version_id = "x"

        def complete(self, system, user, max_tokens=512):
            raise RuntimeError("llm down")

    sess = _session(llm=BrokenLLM())
    events = sess.add_audio(_tone(2.0)) + sess.add_audio(_silence(1.0))
    replies = [e for e in events if e["type"] == "bot_reply"]
    assert replies and replies[0].get("warnings") == ["llm_unavailable"]
    # no tts_audio when there's no reply
    assert not [e for e in events if e["type"] == "tts_audio"]


def test_converse_route_e2e():
    from fastapi.testclient import TestClient

    from orpheus_workers.streaming import create_app

    # inject the complete transcriber; llm/tts default to stubs via get_llm/get_tts
    app = create_app(transcriber=_complete_tx, partial_transcriber=_complete_tx)
    client = TestClient(app)
    with client.websocket_connect("/v1/stream/converse") as ws:
        ws.send_json({"type": "start", "sample_rate": SR})
        assert ws.receive_json()["type"] == "ready"
        ws.send_bytes(_tone(2.0))
        ws.send_bytes(_silence(1.0))
        ws.send_json({"type": "finalize"})
        got = []
        while True:
            ev = ws.receive_json()
            got.append(ev)
            if ev["type"] == "done":
                break
    types = {e["type"] for e in got}
    assert "bot_reply" in types  # the agent responded end-to-end
    assert "tts_audio" in types  # stub TTS produced audio
