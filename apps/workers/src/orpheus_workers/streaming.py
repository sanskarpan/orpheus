"""Realtime streaming transcription over WebSockets (gap #12).

A client opens a WebSocket, optionally sends a ``start`` control frame, then
streams raw PCM audio (16-bit signed little-endian, mono) as binary frames.
The server transcribes incrementally and streams results back:

  - ``partial`` — a provisional transcript of the un-confirmed tail. It may
    change on the next update; clients render it as "in progress".
  - ``final``   — confirmed words, never re-sent, never dropped.
  - ``done``    — sent after the client finalizes; carries the full confirmed
    transcript, then the socket closes.

**Decoding — LocalAgreement-2.** Naively re-transcribing a growing window makes
partials unstable and wastes compute. Instead we re-decode the working buffer
and *confirm* the longest word prefix that two consecutive hypotheses agree on
(LocalAgreement-2, from Whisper-Streaming). Confirmed words are emitted as
finals; the buffer is then trimmed at the last confirmed word so re-transcription
stays bounded rather than growing with the session. A lightweight energy **VAD**
detects trailing silence and flushes/endpoints at natural pauses (and forces a
trim so a long pause never grows the buffer).

The state machine (:class:`StreamSession`) is transport-free and unit-testable;
the transcriber is injectable so tests run without Whisper. The transcriber
returns word-level timestamps (``{"words": [{"word","start","end"}, ...]}``).
See docs/design/12-streaming-realtime.md.
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

# A transcriber turns a PCM16 mono buffer into a result dict carrying word-level
# timestamps: ``{"words": [{"word", "start", "end"}, ...], "text": ...}``. The
# default uses Whisper; tests inject a fake.
Transcriber = Callable[[bytes, int], dict]

_BYTES_PER_SAMPLE = 2  # 16-bit mono


def pcm16_to_wav_file(pcm: bytes, sample_rate: int, path: str | Path) -> None:
    """Write raw PCM16 mono samples to a wav file Whisper can read."""
    with wave.open(str(path), "wb") as w:
        w.setnchannels(1)
        w.setsampwidth(_BYTES_PER_SAMPLE)
        w.setframerate(sample_rate)
        w.writeframes(pcm)


def _flatten_words(result: dict) -> list[dict[str, Any]]:
    """Pull ``[{word,start,end}]`` from a transcribe result's segments/words."""
    words: list[dict[str, Any]] = []
    for seg in result.get("segments") or []:
        for w in seg.get("words") or []:
            text = (w.get("word") or "").strip()
            if text:
                words.append(
                    {
                        "word": text,
                        "start": float(w.get("start", 0.0)),
                        "end": float(w.get("end", 0.0)),
                    }
                )
    return words


def whisper_transcriber(pcm: bytes, sample_rate: int) -> dict:
    """Default transcriber: dump the PCM buffer to a temp wav and run Whisper
    with word timestamps. Imported lazily so the module loads without the model.
    """
    from .transcribe import transcribe as run_whisper

    model_size = os.environ.get("ORPHEUS_WORKER_WHISPER_MODEL", "tiny.en")
    with tempfile.NamedTemporaryFile(suffix=".wav", delete=False) as tf:
        tmp = tf.name
    try:
        pcm16_to_wav_file(pcm, sample_rate, tmp)
        res = run_whisper(tmp, model_size=model_size, word_timestamps=True)
        res.setdefault("words", _flatten_words(res))
        return res
    finally:
        with contextlib.suppress(FileNotFoundError):
            os.unlink(tmp)


def _norm(word: str) -> str:
    """Normalize a word for agreement comparison (case/punctuation-insensitive)."""
    return "".join(c for c in word.lower() if c.isalnum())


@dataclass
class StreamConfig:
    sample_rate: int = 16_000
    # Re-decode once this many seconds of new audio have arrived.
    min_chunk_seconds: float = 1.0
    # Force a buffer trim once it grows this long, so re-transcription is bounded
    # even with continuous speech and no agreement.
    max_buffer_seconds: float = 20.0
    # Trailing audio quieter than vad_energy_ratio × the buffer's RMS for at
    # least this long is treated as a pause → flush + endpoint.
    vad_silence_seconds: float = 0.6
    vad_energy_ratio: float = 0.30
    # Set False to disable VAD endpointing (deterministic tests).
    vad_enabled: bool = True

    @property
    def min_chunk_samples(self) -> int:
        return int(self.min_chunk_seconds * self.sample_rate)

    @property
    def max_buffer_samples(self) -> int:
        return int(self.max_buffer_seconds * self.sample_rate)


def _extract_words(result: dict) -> list[dict[str, Any]]:
    words = result.get("words")
    if words is None:
        words = _flatten_words(result)
    return [
        {
            "word": str(w.get("word", "")).strip(),
            "start": float(w.get("start", 0.0)),
            "end": float(w.get("end", 0.0)),
        }
        for w in words
        if str(w.get("word", "")).strip()
    ]


@dataclass
class StreamSession:
    """LocalAgreement-2 state machine for streaming transcription.

    Feed audio with :meth:`add_audio`; it returns the events to send. Call
    :meth:`finalize` when the client is done. Confirmed words are emitted once,
    in order; the buffer is trimmed at confirmations so cost stays bounded.
    """

    transcriber: Transcriber
    config: StreamConfig = field(default_factory=StreamConfig)
    _buffer: bytearray = field(default_factory=bytearray)  # audio after _offset_s
    _offset_s: float = 0.0  # absolute time already trimmed away
    _committed: int = 0  # words confirmed within the CURRENT buffer
    _prev: list[str] = field(default_factory=list)  # last hypothesis (normalized)
    _final_texts: list[str] = field(default_factory=list)
    _samples_since_decode: int = 0

    @property
    def _buffer_samples(self) -> int:
        return len(self._buffer) // _BYTES_PER_SAMPLE

    def add_audio(self, pcm: bytes) -> list[dict[str, Any]]:
        self._buffer.extend(pcm)
        self._samples_since_decode += len(pcm) // _BYTES_PER_SAMPLE
        if self._samples_since_decode < self.config.min_chunk_samples:
            return []
        self._samples_since_decode = 0
        return self._decode(flush=False)

    def finalize(self) -> list[dict[str, Any]]:
        events = self._decode(flush=True)
        events.append({"type": "done", "text": " ".join(self._final_texts).strip()})
        return events

    def _decode(self, flush: bool) -> list[dict[str, Any]]:
        events: list[dict[str, Any]] = []
        if self._buffer_samples == 0:
            return events

        result = self.transcriber(bytes(self._buffer), self.config.sample_rate)
        words = _extract_words(result)

        # LocalAgreement-2: confirm the longest prefix the last two hypotheses
        # agree on. On flush (finalize) or a detected pause, confirm everything.
        cur = [_norm(w["word"]) for w in words]
        agreed = _common_prefix_len(self._prev, cur)
        endpoint = flush or (self.config.vad_enabled and self._trailing_silence())
        confirm_to = len(words) if endpoint else agreed

        if confirm_to > self._committed:
            newly = words[self._committed : confirm_to]
            text = " ".join(w["word"] for w in newly).strip()
            if text:
                self._final_texts.append(text)
                events.append(
                    {
                        "type": "final",
                        "text": text,
                        "start": self._offset_s + newly[0]["start"],
                        "end": self._offset_s + newly[-1]["end"],
                    }
                )
            self._committed = confirm_to

        self._prev = cur

        # Trim: on an endpoint, or when the buffer is too long, drop everything
        # up to the last confirmed word so re-transcription stays bounded.
        if (
            endpoint or self._buffer_samples >= self.config.max_buffer_samples
        ) and self._committed > 0:
            cut_s = words[self._committed - 1]["end"]
            cut_samples = min(int(cut_s * self.config.sample_rate), self._buffer_samples)
            del self._buffer[: cut_samples * _BYTES_PER_SAMPLE]
            self._offset_s += cut_samples / self.config.sample_rate
            self._committed = 0
            self._prev = []
            words = []  # tail already trimmed away

        # Provisional partial for the still-unconfirmed tail.
        tail = words[self._committed :]
        if tail and not flush:
            events.append(
                {
                    "type": "partial",
                    "text": " ".join(w["word"] for w in tail).strip(),
                    "start": self._offset_s + tail[0]["start"],
                }
            )
        return events

    def _trailing_silence(self) -> bool:
        """True when the buffer ends with vad_silence_seconds of low energy."""
        import numpy as np

        n = int(self.config.vad_silence_seconds * self.config.sample_rate)
        if self._buffer_samples < n * 2:  # need speech + trailing region to compare
            return False
        buf = np.frombuffer(bytes(self._buffer), dtype=np.int16).astype(np.float32)
        whole_rms = float(np.sqrt(np.mean(buf**2)))
        tail_rms = float(np.sqrt(np.mean(buf[-n:] ** 2)))
        return whole_rms > 0 and tail_rms < self.config.vad_energy_ratio * whole_rms


def _common_prefix_len(a: list[str], b: list[str]) -> int:
    n = 0
    for x, y in zip(a, b):
        if x != y or not x:
            break
        n += 1
    return n


def create_app(transcriber: Transcriber | None = None) -> Any:
    """Build the FastAPI app exposing the streaming WebSocket."""
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
