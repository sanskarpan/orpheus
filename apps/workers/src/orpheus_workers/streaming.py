"""Realtime streaming transcription over WebSockets (gap #12).

A client opens a WebSocket, optionally sends a ``start`` control frame, then
streams raw PCM audio (16-bit signed little-endian, mono) as binary frames.
The server runs chunked Whisper over a rolling buffer and streams results
back:

  - ``partial`` — a provisional transcript of the un-finalized tail. It may
    change on the next update; clients render it as "in progress" and are
    free to drop it under backpressure.
  - ``final``   — a stable transcript for a completed window. Never re-sent,
    never dropped.
  - ``done``    — sent after the client finalizes; carries the full
    concatenated final transcript, then the socket closes.

The state machine (:class:`StreamSession`) is deliberately transport-free so
it is unit-testable without a socket, and the transcriber is injectable so
tests run without the Whisper model. See docs/design/12-streaming-realtime.md.
"""

from __future__ import annotations

import contextlib
import json
import os
import tempfile
import wave
from collections.abc import Callable
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

# Imported at module level (not lazily) so that with `from __future__ import
# annotations` FastAPI can resolve the `ws: WebSocket` endpoint annotation
# against module globals — otherwise it misreads `ws` as a query parameter.
from fastapi import FastAPI, WebSocket, WebSocketDisconnect

# A transcriber turns a PCM16 mono buffer into a result dict with at least a
# "text" key. The default implementation uses Whisper; tests inject a fake.
Transcriber = Callable[[bytes, int], dict]

_BYTES_PER_SAMPLE = 2  # 16-bit mono


def pcm16_to_wav_file(pcm: bytes, sample_rate: int, path: str | Path) -> None:
    """Write raw PCM16 mono samples to a wav file Whisper can read."""
    with wave.open(str(path), "wb") as w:
        w.setnchannels(1)
        w.setsampwidth(_BYTES_PER_SAMPLE)
        w.setframerate(sample_rate)
        w.writeframes(pcm)


def whisper_transcriber(pcm: bytes, sample_rate: int) -> dict:
    """Default transcriber: dump the PCM window to a temp wav and run Whisper.

    Imported lazily so the module (and its tests) load without the model.
    """
    from .transcribe import transcribe as run_whisper

    model_size = os.environ.get("ORPHEUS_WORKER_WHISPER_MODEL", "tiny.en")
    with tempfile.NamedTemporaryFile(suffix=".wav", delete=False) as tf:
        tmp = tf.name
    try:
        pcm16_to_wav_file(pcm, sample_rate, tmp)
        return run_whisper(tmp, model_size=model_size)
    finally:
        with contextlib.suppress(FileNotFoundError):
            os.unlink(tmp)


@dataclass
class StreamConfig:
    sample_rate: int = 16_000
    # A window is finalized once this many seconds of un-finalized audio have
    # accumulated. Larger = fewer, more-accurate finals but higher latency.
    window_seconds: float = 3.0
    # Emit a fresh partial once this many new seconds have arrived since the
    # last partial. Smaller = snappier interims, more compute.
    partial_interval_seconds: float = 1.0

    @property
    def window_samples(self) -> int:
        return int(self.window_seconds * self.sample_rate)

    @property
    def partial_interval_samples(self) -> int:
        return int(self.partial_interval_seconds * self.sample_rate)


@dataclass
class StreamSession:
    """Rolling-buffer state machine for streaming transcription.

    Feed it audio with :meth:`add_audio`; it returns the events to send. Call
    :meth:`finalize` when the client is done. It guarantees finals are emitted
    once, in order, and cover the whole stream exactly once.
    """

    transcriber: Transcriber
    config: StreamConfig = field(default_factory=StreamConfig)
    _buffer: bytearray = field(default_factory=bytearray)
    _finalized_samples: int = 0
    _last_partial_samples: int = 0
    _final_texts: list[str] = field(default_factory=list)

    @property
    def _total_samples(self) -> int:
        return len(self._buffer) // _BYTES_PER_SAMPLE

    def add_audio(self, pcm: bytes) -> list[dict[str, Any]]:
        """Append audio and return any partial/final events it produced."""
        self._buffer.extend(pcm)
        events: list[dict[str, Any]] = []

        # Finalize every completed window first (stable, never re-sent).
        win = self.config.window_samples
        while self._total_samples - self._finalized_samples >= win and win > 0:
            events.append(self._finalize_range(self._finalized_samples + win))

        # Then a provisional partial for the remaining tail, rate-limited.
        new_since_partial = self._total_samples - self._last_partial_samples
        if (
            self._total_samples > self._finalized_samples
            and new_since_partial >= self.config.partial_interval_samples
        ):
            tail = bytes(self._buffer[self._finalized_samples * _BYTES_PER_SAMPLE :])
            res = self.transcriber(tail, self.config.sample_rate)
            self._last_partial_samples = self._total_samples
            events.append(
                {
                    "type": "partial",
                    "text": res.get("text", ""),
                    "start": self._finalized_samples / self.config.sample_rate,
                }
            )
        return events

    def finalize(self) -> list[dict[str, Any]]:
        """Finalize any remaining tail and return the final(s) + ``done``."""
        events: list[dict[str, Any]] = []
        if self._total_samples > self._finalized_samples:
            events.append(self._finalize_range(self._total_samples))
        events.append({"type": "done", "text": " ".join(self._final_texts).strip()})
        return events

    def _finalize_range(self, end_samples: int) -> dict[str, Any]:
        """Transcribe [_finalized_samples, end_samples) as a stable final."""
        start = self._finalized_samples
        seg = bytes(self._buffer[start * _BYTES_PER_SAMPLE : end_samples * _BYTES_PER_SAMPLE])
        res = self.transcriber(seg, self.config.sample_rate)
        text = res.get("text", "")
        self._final_texts.append(text)
        self._finalized_samples = end_samples
        self._last_partial_samples = end_samples
        return {
            "type": "final",
            "text": text,
            "start": start / self.config.sample_rate,
            "end": end_samples / self.config.sample_rate,
        }


def create_app(transcriber: Transcriber | None = None) -> Any:
    """Build the FastAPI app exposing the streaming WebSocket.

    ``transcriber`` defaults to Whisper; tests pass a fake.
    """
    tx = transcriber or whisper_transcriber
    app = FastAPI(title="Orpheus Streaming", version="0.1.0")

    @app.get("/health")
    async def health() -> dict[str, str]:
        return {"status": "ok"}

    @app.websocket("/v1/stream/transcribe")
    async def stream_transcribe(ws: WebSocket) -> None:
        await ws.accept()
        config = StreamConfig()
        session: StreamSession | None = None

        async def ensure_session() -> StreamSession:
            nonlocal session
            if session is None:
                session = StreamSession(transcriber=tx, config=config)
            return session

        try:
            while True:
                msg = await ws.receive()
                if msg["type"] == "websocket.disconnect":
                    break

                if (data := msg.get("bytes")) is not None:
                    sess = await ensure_session()
                    for ev in sess.add_audio(data):
                        await ws.send_text(json.dumps(ev))
                    continue

                text = msg.get("text")
                if text is None:
                    continue
                try:
                    control = json.loads(text)
                except json.JSONDecodeError:
                    await ws.send_text(json.dumps({"type": "error", "error": "invalid json"}))
                    continue

                ctype = control.get("type")
                if ctype == "start":
                    if isinstance(control.get("sample_rate"), int):
                        config.sample_rate = control["sample_rate"]
                    session = None  # reset for a fresh stream
                    await ws.send_text(json.dumps({"type": "ready"}))
                elif ctype in ("finalize", "stop", "close"):
                    sess = await ensure_session()
                    for ev in sess.finalize():
                        await ws.send_text(json.dumps(ev))
                    break
                else:
                    await ws.send_text(
                        json.dumps({"type": "error", "error": f"unknown control {ctype!r}"})
                    )
        except WebSocketDisconnect:
            pass
        finally:
            with contextlib.suppress(RuntimeError):
                await ws.close()

    return app


# Module-level app for `uvicorn orpheus_workers.streaming:app`.
app = None


def _get_app() -> Any:
    global app
    if app is None:
        app = create_app()
    return app


def main() -> None:
    """Run the streaming server (production entrypoint)."""
    import uvicorn

    host = os.environ.get("ORPHEUS_STREAMING_HOST", "0.0.0.0")
    port = int(os.environ.get("ORPHEUS_STREAMING_PORT", "8082"))
    uvicorn.run(_get_app(), host=host, port=port)


if __name__ == "__main__":
    main()
