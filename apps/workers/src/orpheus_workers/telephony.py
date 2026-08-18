"""Telephony ingestion (PRD 05 #355) — Twilio Media Streams bridge.

Carriers don't speak WebRTC/PCM16 directly; the standard integration is a media
gateway (Twilio/Telnyx/LiveKit SIP) that terminates the SIP/RTP call and streams
the audio to an application WebSocket. This module implements the **Twilio Media
Streams** protocol (the most common such bridge): G.711 μ-law 8 kHz frames in/out,
DTMF, and a ``clear`` for barge-in — transcoding to the same PCM16 16 kHz pipeline
the browser uses (``streaming``/``converse``).

Pure functions here (μ-law codec, resampling, frame parsing/building) are
dependency-light and unit-tested; the WS route in ``streaming.create_app`` drives
them. Going *live* needs a Twilio number + phone-number webhook pointing at the
route — the only external dependency.
"""

from __future__ import annotations

import base64
from typing import Any

_ULAW_BIAS = 0x84
_ULAW_CLIP = 32635


def _build_ulaw_decode_table() -> list[int]:
    table = []
    for u in range(256):
        u_val = ~u & 0xFF
        sign = u_val & 0x80
        exponent = (u_val >> 4) & 0x07
        mantissa = u_val & 0x0F
        sample = ((mantissa << 3) + _ULAW_BIAS) << exponent
        sample -= _ULAW_BIAS
        table.append(-sample if sign else sample)
    return table


_ULAW_DECODE = _build_ulaw_decode_table()


def ulaw_to_pcm16(payload: bytes) -> Any:
    """Decode G.711 μ-law bytes → int16 numpy array (linear PCM)."""
    import numpy as np

    idx = np.frombuffer(payload, dtype=np.uint8)
    return np.array(_ULAW_DECODE, dtype=np.int16)[idx]


def pcm16_to_ulaw(pcm: Any) -> bytes:
    """Encode an int16 numpy array → G.711 μ-law bytes."""
    import numpy as np

    x = np.asarray(pcm, dtype=np.int32)
    sign = (x < 0).astype(np.int32) * 0x80
    mag = np.clip(np.abs(x), 0, _ULAW_CLIP) + _ULAW_BIAS
    exponent = np.zeros_like(mag)
    # Ascending so the *highest* qualifying exponent is the one kept.
    for e in range(8):
        exponent = np.where(mag >= (1 << (e + 7)), e, exponent)
    mantissa = (mag >> (exponent + 3)) & 0x0F
    ulaw = ~(sign | (exponent << 4) | mantissa) & 0xFF
    return ulaw.astype(np.uint8).tobytes()


def resample_linear(pcm: Any, src_rate: int, dst_rate: int) -> Any:
    """Linear-interpolation resample of an int16 numpy array."""
    import numpy as np

    if src_rate == dst_rate or len(pcm) == 0:
        return np.asarray(pcm, dtype=np.int16)
    x = np.asarray(pcm, dtype=np.float32)
    n_out = int(round(len(x) * dst_rate / src_rate))
    if n_out <= 0:
        return np.zeros(0, dtype=np.int16)
    src_idx = np.linspace(0, len(x) - 1, n_out)
    out = np.interp(src_idx, np.arange(len(x)), x)
    return np.clip(out, -32768, 32767).astype(np.int16)


def twilio_media_to_pcm16_16k(payload_b64: str) -> bytes:
    """Twilio inbound media (base64 μ-law 8 kHz) → PCM16 16 kHz bytes."""
    ulaw = base64.b64decode(payload_b64)
    pcm8 = ulaw_to_pcm16(ulaw)
    pcm16 = resample_linear(pcm8, 8000, 16000)
    return pcm16.tobytes()


def pcm16_any_to_twilio_media(pcm_bytes: bytes, src_rate: int, stream_sid: str) -> list[dict]:
    """PCM16 @ src_rate → Twilio outbound ``media`` frames (μ-law 8 kHz, 20 ms).

    Returns a list of ``{"event":"media",...}`` dicts ready to json-dump to Twilio.
    Chunked into 160-sample (20 ms @ 8 kHz) payloads, as Twilio expects.
    """
    import numpy as np

    # Guard a misaligned (odd-length) PCM buffer — frombuffer(int16) would raise.
    if len(pcm_bytes) & 1:
        pcm_bytes = pcm_bytes[:-1]
    pcm = np.frombuffer(pcm_bytes, dtype=np.int16)
    pcm8 = resample_linear(pcm, src_rate, 8000)
    ulaw = pcm16_to_ulaw(pcm8)
    frames = []
    for i in range(0, len(ulaw), 160):
        chunk = ulaw[i : i + 160]
        frames.append(
            {
                "event": "media",
                "streamSid": stream_sid,
                "media": {"payload": base64.b64encode(chunk).decode()},
            }
        )
    return frames


def twilio_clear(stream_sid: str) -> dict:
    """A Twilio ``clear`` frame — flush buffered outbound audio (barge-in)."""
    return {"event": "clear", "streamSid": stream_sid}


def parse_twilio_frame(msg: dict) -> dict[str, Any]:
    """Normalize an inbound Twilio Media Streams frame to a small internal shape."""
    event = msg.get("event")
    if event == "start":
        start = msg.get("start", {})
        return {"kind": "start", "stream_sid": start.get("streamSid") or msg.get("streamSid")}
    if event == "media":
        return {"kind": "media", "payload": (msg.get("media") or {}).get("payload", "")}
    if event == "dtmf":
        return {"kind": "dtmf", "digit": (msg.get("dtmf") or {}).get("digit", "")}
    if event == "stop":
        return {"kind": "stop"}
    if event == "mark":
        return {"kind": "mark", "name": (msg.get("mark") or {}).get("name", "")}
    return {"kind": event or "unknown"}
