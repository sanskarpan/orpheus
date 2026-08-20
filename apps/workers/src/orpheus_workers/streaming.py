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

import base64
import contextlib
import json
import os
import re
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
                        "confidence": float(w.get("confidence", w.get("probability", 0.0)) or 0.0),
                    }
                )
    return words


def _run_whisper_pcm(pcm: bytes, sample_rate: int, model_size: str) -> dict:
    """Dump the PCM buffer to a temp wav and run Whisper with word timestamps.

    Imported lazily so the module loads without the model present.
    """
    from .transcribe import transcribe as run_whisper

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


def whisper_transcriber(pcm: bytes, sample_rate: int) -> dict:
    """Commit-cadence transcriber: the accurate model that confirms finals."""
    model_size = os.environ.get("ORPHEUS_WORKER_WHISPER_MODEL", "tiny.en")
    return _run_whisper_pcm(pcm, sample_rate, model_size)


def partial_whisper_transcriber(pcm: bytes, sample_rate: int) -> dict:
    """Fast partial-cadence transcriber: a small model that keeps the sub-300ms
    provisional loop cheap. Defaults to ``ORPHEUS_STREAMING_PARTIAL_MODEL`` (else
    the commit model), so a deployment can run e.g. ``tiny.en`` for partials and
    ``large-v3-turbo`` for finals.
    """
    model_size = (
        os.environ.get("ORPHEUS_STREAMING_PARTIAL_MODEL")
        or os.environ.get("ORPHEUS_WORKER_WHISPER_MODEL", "tiny.en")
    )
    return _run_whisper_pcm(pcm, sample_rate, model_size)


# A speaker embedder turns a PCM16 mono buffer into a fixed-length voice
# embedding (or None if the clip is unusable). The default uses SpeechBrain
# ECAPA; tests inject a fake. Online clustering assigns each confirmed segment
# to the nearest known speaker centroid.
Embedder = Callable[[bytes, int], "list[float] | None"]

_ecapa_model: Any = None


def ecapa_embedder(pcm: bytes, sample_rate: int) -> list[float] | None:
    """Default speaker embedder: SpeechBrain ECAPA-TDNN (VoxCeleb).

    Lazy-imported and cached so the module loads without torch/speechbrain. The
    same 192-d embedding family the offline Modal diarizer uses, so streaming and
    batch speaker geometry stay comparable. Returns None on any failure.
    """
    global _ecapa_model
    try:
        import numpy as np
        import torch

        if _ecapa_model is None:
            from speechbrain.inference.speaker import EncoderClassifier

            source = os.environ.get(
                "ORPHEUS_STREAMING_EMBED_MODEL", "speechbrain/spkrec-ecapa-voxceleb"
            )
            _ecapa_model = EncoderClassifier.from_hparams(source=source)
        samples = np.frombuffer(pcm, dtype=np.int16).astype(np.float32) / 32768.0
        if samples.size == 0:
            return None
        wav = torch.from_numpy(samples).unsqueeze(0)
        emb = _ecapa_model.encode_batch(wav).squeeze().detach().cpu().numpy()
        return [float(x) for x in np.ravel(emb)]
    except Exception:
        return None


def _norm(word: str) -> str:
    """Normalize a word for agreement comparison (case/punctuation-insensitive)."""
    return "".join(c for c in word.lower() if c.isalnum())


@dataclass
class StreamConfig:
    sample_rate: int = 16_000
    # Re-decode + run LocalAgreement (the accurate "commit" pass) once this many
    # seconds of new audio have arrived.
    min_chunk_seconds: float = 1.0
    # Fast "partial" cadence: emit a provisional transcript of the un-confirmed
    # tail this often, using the small partial model, so first-glass latency is
    # sub-300ms even though commits stay on the slower, accurate cadence. Set to
    # 0 to disable the fast loop and fall back to commit-cadence partials only.
    partial_chunk_seconds: float = 0.25
    # Don't run a fast partial on a tail shorter than this (not enough signal).
    partial_min_seconds: float = 0.12
    # Force a buffer trim once it grows this long, so re-transcription is bounded
    # even with continuous speech and no agreement.
    max_buffer_seconds: float = 20.0
    # Trailing audio quieter than vad_energy_ratio × the buffer's RMS for at
    # least this long is treated as a pause → flush + endpoint.
    vad_silence_seconds: float = 0.6
    vad_energy_ratio: float = 0.30
    # Set False to disable VAD endpointing (deterministic tests).
    vad_enabled: bool = True
    # End-of-turn (endpoint) backend:
    #   "energy"   — pause of vad_silence_seconds ends the turn (fast, content-blind).
    #   "semantic" — a pause ends the turn only when the utterance also *reads*
    #                complete (terminal punctuation / no dangling word); a mid-
    #                sentence pause is held until turn_max_silence_seconds so we
    #                don't cut a speaker who's mid-thought.
    #   "modal"    — offload the "is this utterance complete?" call to a turn model
    #                endpoint, falling back to semantic when it's unavailable.
    turn_backend: str = "energy"
    # Even mid-sentence, a pause this long always ends the turn (semantic/modal).
    turn_max_silence_seconds: float = 1.5
    # Eager end-of-turn: once the utterance reads complete and a *short* pause of
    # this long is seen (before the firm endpoint confirms), emit a speculative
    # end so a downstream agent can start responding early. If speech resumes
    # before the firm endpoint, a cancel is emitted. 0 disables speculation.
    eager_endpoint_seconds: float = 0.25
    # In-stream diarization: tag each confirmed segment with an anonymous speaker
    # label via online embedding + nearest-centroid clustering.
    diarize: bool = False
    # Cosine similarity to a known speaker's centroid at/above which a segment is
    # that speaker; below it, a new speaker is created. Tuned against real ECAPA
    # geometry over 1.5 s windows (higher over-segments; lower merges speakers).
    diarize_threshold: float = 0.50
    # Don't embed a confirmed region shorter than this (too little voice signal);
    # reuse the currently-active speaker instead.
    diarize_min_seconds: float = 0.5
    # Embed a trailing window at least this long ending at the confirmed region.
    # ECAPA embeddings from sub-second clips are noisy and over-segment; a ~1.5 s
    # window (matching the offline diarizer) gives stable, clusterable geometry
    # even when LocalAgreement only just confirmed a couple of words.
    diarize_window_seconds: float = 1.5
    # Voice-agent turn-taking events (PRD 15): emit speech_start/speech_end/
    # barge_in/backchannel/voicemail alongside transcript events.
    turn_events: bool = False
    # The client sets this (via a bot_state control frame) while the agent's TTS is
    # playing; a user speech_start while it's True raises a barge_in.
    bot_speaking: bool = False

    @property
    def min_chunk_samples(self) -> int:
        return int(self.min_chunk_seconds * self.sample_rate)

    @property
    def partial_chunk_samples(self) -> int:
        return int(self.partial_chunk_seconds * self.sample_rate)

    @property
    def partial_min_samples(self) -> int:
        return int(self.partial_min_seconds * self.sample_rate)

    @property
    def max_buffer_samples(self) -> int:
        return int(self.max_buffer_seconds * self.sample_rate)


# --- end-of-turn (endpoint) detection --------------------------------------
#
# A ``SilenceProbe`` answers "has the buffer ended with >= N seconds of low
# energy?" for a given N. Detectors combine it with the running hypothesis text
# to decide whether the current turn has ended.
SilenceProbe = Callable[[float], bool]

# Words that, when they end an utterance, signal the speaker is mid-thought — so
# a pause after them should NOT end the turn (they're waiting to continue).
_DANGLING_WORDS = {
    "and", "but", "or", "so", "because", "the", "a", "an", "to", "of", "for",
    "in", "on", "at", "with", "my", "your", "our", "their", "his", "her", "its",
    "is", "are", "was", "were", "that", "which", "as", "if", "when", "while",
    "i", "we", "you", "they", "he", "she", "it",
}


def _looks_complete(text: str) -> bool:
    """Heuristic: does this hypothesis read like a finished utterance?

    Complete when it ends in terminal punctuation, or is a few words long and
    doesn't end on a dangling function word (conjunction/article/preposition/…).
    """
    stripped = text.strip()
    if not stripped:
        return False
    if stripped[-1] in ".!?":
        return True
    words = [w for w in re.findall(r"[^\W_]+", stripped.lower())]
    if len(words) < 3:
        return False  # too short to confidently call a complete turn
    return words[-1] not in _DANGLING_WORDS


class EnergyEndpointDetector:
    """Turn ends on a pause of ``vad_silence_seconds`` — content-blind, fastest."""

    def __init__(self, config: StreamConfig) -> None:
        self.config = config

    def is_endpoint(self, tail_text: str, silence_probe: SilenceProbe) -> bool:
        return silence_probe(self.config.vad_silence_seconds)


class SemanticEndpointDetector:
    """Turn ends on a pause only when the utterance also *reads* complete.

    A pause after a complete sentence ends the turn promptly; a pause mid-sentence
    is held until ``turn_max_silence_seconds`` so a thinking speaker isn't cut off.
    """

    def __init__(self, config: StreamConfig) -> None:
        self.config = config

    def _semantic_complete(self, tail_text: str) -> bool:
        return _looks_complete(tail_text)

    def is_endpoint(self, tail_text: str, silence_probe: SilenceProbe) -> bool:
        if not silence_probe(self.config.vad_silence_seconds):
            return False  # still speaking
        if self._semantic_complete(tail_text):
            return True  # complete utterance + a pause → end of turn
        # mid-sentence pause: hold until the pause gets long enough to be a real stop
        return silence_probe(self.config.turn_max_silence_seconds)


class ModalEndpointDetector(SemanticEndpointDetector):
    """Offload the "is this utterance complete?" judgement to a turn model.

    POSTs the hypothesis to ``ORPHEUS_MODAL_TURN_URL``; on any error (unset,
    timeout, 5xx) it degrades to the local semantic heuristic — the stream never
    stalls on the turn service.
    """

    def _semantic_complete(self, tail_text: str) -> bool:
        url = os.environ.get("ORPHEUS_MODAL_TURN_URL", "").strip()
        token = os.environ.get("ORPHEUS_MODAL_TURN_TOKEN", "").strip()
        if not url:
            return super()._semantic_complete(tail_text)
        try:
            import httpx

            resp = httpx.post(
                url, json={"token": token, "text": tail_text}, timeout=2.0
            )
            resp.raise_for_status()
            return bool(resp.json().get("complete"))
        except Exception:
            return super()._semantic_complete(tail_text)  # fail-open to local


def make_endpoint_detector(config: StreamConfig) -> Any:
    backend = (config.turn_backend or "energy").strip().lower()
    if backend == "semantic":
        return SemanticEndpointDetector(config)
    if backend == "modal":
        return ModalEndpointDetector(config)
    return EnergyEndpointDetector(config)


# --- voice-agent turn-taking (PRD 15) ---------------------------------------

# Short acknowledgement tokens that are backchannels ("mm-hm", "yeah") rather than
# real turns — an agent should not treat them as an interruption/turn.
_BACKCHANNEL_WORDS = {
    "mm", "mmm", "mhm", "mmhm", "uhhuh", "uhuh", "hmm", "yeah", "yep", "yup",
    "ok", "okay", "right", "sure", "gotcha", "totally", "exactly", "yes", "no",
    "aha", "ah", "oh", "wow", "nice", "cool", "true",
}
# Phrases that signal an answering machine / voicemail greeting.
_VOICEMAIL_CUES = (
    "leave a message", "after the tone", "after the beep", "at the tone",
    "you've reached", "you have reached", "is not available", "please record",
    "record your message", "voicemail", "unavailable to take your call",
    "please leave your", "not available to take",
)


def _is_backchannel(text: str) -> bool:
    """True when ``text`` is a 1-2 word acknowledgement from the backchannel set."""
    toks = [_norm(t) for t in text.split()]
    toks = [t for t in toks if t]
    return 0 < len(toks) <= 2 and all(t in _BACKCHANNEL_WORDS for t in toks)


def _looks_voicemail(text: str) -> bool:
    low = text.lower()
    return any(cue in low for cue in _VOICEMAIL_CUES)


def _extract_words(result: dict) -> list[dict[str, Any]]:
    words = result.get("words")
    if words is None:
        words = _flatten_words(result)
    return [
        {
            "word": str(w.get("word", "")).strip(),
            "start": float(w.get("start", 0.0)),
            "end": float(w.get("end", 0.0)),
            "confidence": float(w.get("confidence", 0.0) or 0.0),
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
    # Optional fast model for the sub-300ms partial cadence. Falls back to the
    # commit transcriber when None (single-model behavior).
    partial_transcriber: Transcriber | None = None
    # Optional speaker embedder for in-stream diarization (config.diarize).
    embedder: Embedder | None = None
    # Optional PII redaction on the outbound stream: {enabled, entities, mask}.
    redact: dict[str, Any] | None = None
    _buffer: bytearray = field(default_factory=bytearray)  # audio after _offset_s
    _offset_s: float = 0.0  # absolute time already trimmed away
    _committed: int = 0  # words confirmed within the CURRENT buffer
    _committed_local_end_s: float = 0.0  # buffer-local end time of last confirmed word
    _prev: list[str] = field(default_factory=list)  # last hypothesis (normalized)
    _final_texts: list[str] = field(default_factory=list)
    _samples_since_commit: int = 0
    _samples_since_partial: int = 0
    _redactions: dict[str, int] = field(default_factory=dict)
    _detector: Any = None
    _endpoint_detector: Any = None
    _turn_id: int = 0  # increments on each firm end-of-turn
    _speculative: bool = False  # an eager end-of-turn is outstanding (unconfirmed)
    _in_speech: bool = False  # VAD speech/silence state (turn events)
    _voicemail_emitted: bool = False  # emit the voicemail event at most once
    _speakers: list[dict[str, Any]] = field(default_factory=list)  # {centroid, count}
    _active_speaker: int | None = None

    @property
    def _buffer_samples(self) -> int:
        return len(self._buffer) // _BYTES_PER_SAMPLE

    def _redact_enabled(self) -> bool:
        return bool(self.redact and self.redact.get("enabled"))

    def _abs_words(
        self, ws: list[dict[str, Any]], base: float | None = None
    ) -> list[dict[str, Any]]:
        """Word list with absolute timestamps + confidence, for the wire.

        ``base`` is the absolute time of the word list's local origin; it defaults
        to the buffer origin (``_offset_s``). The fast partial pass transcribes a
        sub-slice of the buffer, so it passes a later base.
        """
        b = self._offset_s if base is None else base
        return [
            {
                "word": w["word"],
                "start": round(b + w["start"], 3),
                "end": round(b + w["end"], 3),
                "confidence": round(float(w.get("confidence", 0.0)), 3),
            }
            for w in ws
        ]

    def _apply_redaction(
        self, ws: list[dict[str, Any]], commit: bool, base: float | None = None
    ) -> tuple[str, list[dict[str, Any]]]:
        """Redact a word list → (redacted_text, redacted_abs_words).

        Detects PII spans on the joined text and masks both the text and any word
        whose character range overlaps a span, so ``words[]`` cannot leak. Counts
        are accumulated only for committed (final) tokens — the streaming
        guarantee is final-only; partials are best-effort.
        """
        abs_words = self._abs_words(ws, base=base)
        text = " ".join(w["word"] for w in ws).strip()
        if not self._redact_enabled():
            return text, abs_words
        from .redact import _mask_for, get_detector

        entities = self.redact.get("entities") or ["EMAIL", "PHONE", "SSN", "CREDIT_CARD"]
        mask = self.redact.get("mask", "type")
        # per-word char offsets in the joined text
        offsets, pos = [], 0
        for i, w in enumerate(ws):
            if i:
                pos += 1  # joining space
            offsets.append((pos, pos + len(w["word"])))
            pos += len(w["word"])
        try:
            if self._detector is None:
                self._detector = get_detector()
            spans = sorted(
                self._detector.detect(text, entities), key=lambda s: s.start, reverse=True
            )
        except Exception:
            # Redaction is enabled but the detector failed — fail SAFE: mask the
            # whole batch rather than emit cleartext PII, and never crash the socket.
            masked = [{**w, "word": "[REDACTED]"} for w in abs_words]
            return "[REDACTED]", masked
        out = text
        for s in spans:
            out = out[: s.start] + _mask_for(s, mask) + out[s.end :]
            if commit:
                self._redactions[s.entity] = self._redactions.get(s.entity, 0) + 1
        for i, (a, b) in enumerate(offsets):
            for s in spans:
                if a < s.end and b > s.start:  # word overlaps a redacted span
                    abs_words[i] = {**abs_words[i], "word": _mask_for(s, mask)}
                    break
        return out, abs_words

    def add_audio(self, pcm: bytes) -> list[dict[str, Any]]:
        n = len(pcm) // _BYTES_PER_SAMPLE
        self._buffer.extend(pcm)
        self._samples_since_commit += n
        self._samples_since_partial += n

        # Turn-taking VAD runs on every chunk (low latency), before decode cadence.
        events: list[dict[str, Any]] = []
        if self.config.turn_events:
            events.extend(self._detect_speech_events())

        # Commit cadence (accurate model): LocalAgreement, finals, trim. This pass
        # also emits its own tail partial, so the fast counter resets with it.
        if self._samples_since_commit >= self.config.min_chunk_samples:
            self._samples_since_commit = 0
            self._samples_since_partial = 0
            events.extend(self._decode(flush=False))
            return events

        # Fast partial cadence (small model): a cheap provisional of the new,
        # still-unconfirmed tail — the sub-300ms first-glass loop.
        if (
            self.config.partial_chunk_seconds > 0
            and self._samples_since_partial >= self.config.partial_chunk_samples
        ):
            self._samples_since_partial = 0
            events.extend(self._fast_partial())
        return events

    def _vad_is_speech(self) -> bool:
        """True when the buffer's trailing ~0.2s carries speech-level energy."""
        import numpy as np

        sr = self.config.sample_rate
        n = int(0.2 * sr)
        if self._buffer_samples < n * 2:
            return self._in_speech  # not enough context → hold current state
        buf = np.frombuffer(bytes(self._buffer), dtype=np.int16).astype(np.float32)
        whole_rms = float(np.sqrt(np.mean(buf**2)))
        tail_rms = float(np.sqrt(np.mean(buf[-n:] ** 2)))
        return whole_rms > 0 and tail_rms >= self.config.vad_energy_ratio * whole_rms

    def _detect_speech_events(self) -> list[dict[str, Any]]:
        """Emit speech_start / speech_end (+ barge_in) on VAD state transitions."""
        events: list[dict[str, Any]] = []
        now = round(self._offset_s + self._buffer_samples / self.config.sample_rate, 3)
        speech = self._vad_is_speech()
        if speech and not self._in_speech:
            self._in_speech = True
            events.append({"type": "speech_start", "time": now})
            if self.config.bot_speaking:
                # user talked over the agent's TTS → interruption
                events.append({"type": "barge_in", "turn_id": self._turn_id, "time": now})
        elif not speech and self._in_speech and self._trailing_silence(
            self.config.vad_silence_seconds
        ):
            self._in_speech = False
            events.append({"type": "speech_end", "time": now})
        return events

    def _fast_partial(self) -> list[dict[str, Any]]:
        """Transcribe only the un-confirmed tail with the fast model → one partial.

        Operates on the buffer slice *after* the last confirmed word so it never
        re-decodes committed audio, and never mutates commit/agreement state — it
        purely refreshes the provisional view between commits.
        """
        from_sample = int(self._committed_local_end_s * self.config.sample_rate)
        from_byte = min(from_sample * _BYTES_PER_SAMPLE, len(self._buffer))
        tail_pcm = bytes(self._buffer[from_byte:])
        if len(tail_pcm) // _BYTES_PER_SAMPLE < self.config.partial_min_samples:
            return []
        tx = self.partial_transcriber or self.transcriber
        try:
            result = tx(tail_pcm, self.config.sample_rate)
        except Exception:
            return []  # a partial is best-effort; never break the stream on it
        words = _extract_words(result)
        if not words:
            return []
        base = self._offset_s + self._committed_local_end_s
        red_text, red_words = self._apply_redaction(words, commit=False, base=base)
        if not red_text:
            return []
        ev: dict[str, Any] = {
            "type": "partial",
            "text": red_text,
            "start": round(base + words[0]["start"], 3),
            "words": red_words,
            "turn_id": self._turn_id,
        }
        if self._redact_enabled():
            ev["redacted_provisional"] = True
        return [ev]

    def finalize(self) -> list[dict[str, Any]]:
        events = self._decode(flush=True)
        done: dict[str, Any] = {
            "type": "done",
            "text": " ".join(self._final_texts).strip(),
            "turn_id": self._turn_id,
        }
        if self._redactions:
            done["redactions"] = [{"entity": k, "count": v} for k, v in self._redactions.items()]
        events.append(done)
        return events

    def _decode(self, flush: bool) -> list[dict[str, Any]]:
        events: list[dict[str, Any]] = []
        if self._buffer_samples == 0:
            return events

        try:
            result = self.transcriber(bytes(self._buffer), self.config.sample_rate)
        except Exception:
            # A transient model hiccup must not tear down the socket / lose session
            # state — skip this decode (buffer is retained; the backstop bounds it).
            return events
        words = _extract_words(result)
        # A re-decode can return FEWER words than were already committed (real
        # models revise/merge words near the buffer edge). Clamp so every
        # downstream index into `words` (trim, tail, committed_end) stays valid.
        if self._committed > len(words):
            self._committed = len(words)

        # LocalAgreement-2: confirm the longest prefix the last two hypotheses
        # agree on. On flush (finalize) or a detected pause, confirm everything.
        cur = [_norm(w["word"]) for w in words]
        agreed = _common_prefix_len(self._prev, cur)
        # Backstop: once the buffer hits max_buffer_seconds, force progress even
        # without agreement/endpoint, so continuous speech with an unstable
        # hypothesis can't grow the buffer (and re-decode cost) without bound.
        force_trim = self._buffer_samples >= self.config.max_buffer_samples
        if flush:
            endpoint = True
        elif self.config.vad_enabled:
            hyp_text = " ".join(w["word"] for w in words)
            endpoint = self._endpoint().is_endpoint(hyp_text, self._trailing_silence)
        else:
            endpoint = False
        confirm_to = len(words) if (endpoint or force_trim) else agreed

        if confirm_to > self._committed:
            newly = words[self._committed : confirm_to]
            if newly:
                red_text, red_words = self._apply_redaction(newly, commit=True)
                if red_text:
                    self._final_texts.append(red_text)
                    # In-stream diarization: attribute this confirmed region to a
                    # speaker (must run before the buffer is trimmed below).
                    spk = self._assign_speaker(newly[0]["start"], newly[-1]["end"])
                    if spk is not None and spk != self._active_speaker:
                        self._active_speaker = spk
                        events.append(
                            {"type": "speaker_update", "speaker": f"S{spk + 1}",
                             "turn_id": self._turn_id}
                        )
                    final_ev: dict[str, Any] = {
                        "type": "final",
                        "text": red_text,
                        "start": round(self._offset_s + newly[0]["start"], 3),
                        "end": round(self._offset_s + newly[-1]["end"], 3),
                        "words": red_words,
                        "turn_id": self._turn_id,
                    }
                    if spk is not None:
                        final_ev["speaker"] = f"S{spk + 1}"
                    events.append(final_ev)
                    # Voice-agent turn-taking tags (PRD 15).
                    if self.config.turn_events:
                        if _is_backchannel(red_text):
                            events.append(
                                {"type": "backchannel", "text": red_text,
                                 "turn_id": self._turn_id}
                            )
                        if not self._voicemail_emitted and _looks_voicemail(
                            " ".join(self._final_texts)
                        ):
                            self._voicemail_emitted = True
                            events.append({"type": "voicemail", "turn_id": self._turn_id})
            self._committed = confirm_to
            # Remember where confirmed audio ends so the fast partial pass can
            # decode only the tail after it.
            self._committed_local_end_s = words[self._committed - 1]["end"]

        self._prev = cur

        # Eager end-of-turn: speculate/cancel so a downstream agent gets a head
        # start before the firm endpoint. Skipped on flush (finalize is firm).
        if not flush and self.config.vad_enabled and self.config.eager_endpoint_seconds > 0:
            if not endpoint:
                hyp_text = " ".join(w["word"] for w in words)
                speculate = _looks_complete(hyp_text) and self._trailing_silence(
                    self.config.eager_endpoint_seconds
                )
                if speculate and not self._speculative:
                    self._speculative = True
                    events.append({"type": "endpoint_speculative", "turn_id": self._turn_id})
                elif not speculate and self._speculative:
                    # the pause broke / speech resumed before the firm endpoint
                    self._speculative = False
                    events.append({"type": "endpoint_cancel", "turn_id": self._turn_id})

        # Firm end-of-turn: authoritative boundary. Confirms the outstanding
        # speculation and advances the turn counter.
        if endpoint and not flush:
            events.append({"type": "endpoint", "turn_id": self._turn_id})
            self._turn_id += 1
            self._speculative = False

        # Trim: on an endpoint, or when the buffer is too long, drop everything
        # up to the last confirmed word so re-transcription stays bounded.
        if (endpoint or force_trim) and self._committed > 0:
            cut_s = words[self._committed - 1]["end"]
            cut_samples = min(int(cut_s * self.config.sample_rate), self._buffer_samples)
            del self._buffer[: cut_samples * _BYTES_PER_SAMPLE]
            self._offset_s += cut_samples / self.config.sample_rate
            self._committed = 0
            self._committed_local_end_s = 0.0
            self._prev = []
            words = []  # tail already trimmed away

        # Hard backstop: if the buffer is STILL over the cap (e.g. the model
        # returned no words at all on noise, so there was nothing to trim at),
        # drop the oldest audio down to half the cap so memory/cost stay bounded.
        if self._buffer_samples >= self.config.max_buffer_samples:
            drop = self._buffer_samples - int(self.config.max_buffer_samples * 0.5)
            if drop > 0:
                del self._buffer[: drop * _BYTES_PER_SAMPLE]
                self._offset_s += drop / self.config.sample_rate
                self._committed = 0
                self._committed_local_end_s = 0.0
                self._prev = []
                words = []

        # Provisional partial for the still-unconfirmed tail.
        tail = words[self._committed :]
        if tail and not flush:
            red_text, red_words = self._apply_redaction(tail, commit=False)
            ev: dict[str, Any] = {
                "type": "partial",
                "text": red_text,
                "start": round(self._offset_s + tail[0]["start"], 3),
                "words": red_words,
                "turn_id": self._turn_id,
            }
            if self._redact_enabled():
                ev["redacted_provisional"] = True  # a partial may still leak until confirmed
            events.append(ev)
        return events

    def _endpoint(self) -> Any:
        if self._endpoint_detector is None:
            self._endpoint_detector = make_endpoint_detector(self.config)
        return self._endpoint_detector

    def _diarize_enabled(self) -> bool:
        return bool(self.config.diarize and self.embedder is not None)

    def _assign_speaker(self, local_start_s: float, local_end_s: float) -> int | None:
        """Online-cluster a confirmed audio region → speaker index (0-based).

        Embeds the region's PCM, cosine-matches it to known speaker centroids, and
        either updates the nearest (running mean) or opens a new speaker. Regions
        too short to embed reuse the currently-active speaker. Any embedder failure
        degrades to the active speaker so diarization never breaks the stream.
        """
        if not self._diarize_enabled():
            return None
        import numpy as np

        sr = self.config.sample_rate
        b = min(self._buffer_samples, int(local_end_s * sr))
        # Embed a trailing window ending at the confirmed region: extend back to at
        # least diarize_window_seconds so a short just-confirmed group still yields
        # a stable ECAPA embedding (short clips over-segment). Never start before
        # the region itself would begin if that region is already long enough.
        win_start_s = min(local_start_s, max(0.0, local_end_s - self.config.diarize_window_seconds))
        a = max(0, int(win_start_s * sr))
        if (b - a) < int(self.config.diarize_min_seconds * sr):
            return self._active_speaker
        pcm = bytes(self._buffer[a * _BYTES_PER_SAMPLE : b * _BYTES_PER_SAMPLE])
        try:
            emb = self.embedder(pcm, sr)
        except Exception:
            return self._active_speaker
        if not emb:
            return self._active_speaker
        v = np.asarray(emb, dtype=np.float32)
        norm = float(np.linalg.norm(v))
        if norm == 0.0:
            return self._active_speaker
        v = v / norm
        best_i, best_sim = None, -1.0
        for i, sp in enumerate(self._speakers):
            sim = float(np.dot(v, sp["centroid"]))
            if sim > best_sim:
                best_i, best_sim = i, sim
        if best_i is not None and best_sim >= self.config.diarize_threshold:
            sp = self._speakers[best_i]
            merged = sp["centroid"] * sp["count"] + v
            mn = float(np.linalg.norm(merged))
            sp["centroid"] = merged / mn if mn else sp["centroid"]
            sp["count"] += 1
            return best_i
        self._speakers.append({"centroid": v, "count": 1})
        return len(self._speakers) - 1

    def _trailing_silence(self, seconds: float | None = None) -> bool:
        """True when the buffer ends with ``seconds`` of low energy.

        ``seconds`` defaults to ``vad_silence_seconds``; endpoint detectors probe
        multiple durations (e.g. a longer hold for a mid-sentence pause).
        """
        import numpy as np

        dur = self.config.vad_silence_seconds if seconds is None else seconds
        n = int(dur * self.config.sample_rate)
        if n <= 0 or self._buffer_samples < n * 2:  # need speech + trailing region
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


def create_app(
    transcriber: Transcriber | None = None,
    partial_transcriber: Transcriber | None = None,
    embedder: Embedder | None = None,
) -> Any:
    """Build the FastAPI app exposing the streaming WebSocket.

    ``transcriber`` runs the accurate commit cadence; ``partial_transcriber``
    runs the fast sub-300ms partial cadence (defaults to the small partial model);
    ``embedder`` powers in-stream diarization (defaults to ECAPA when the server
    has ``ORPHEUS_STREAMING_DIARIZE`` enabled).
    """
    tx = transcriber or whisper_transcriber
    partial_tx = partial_transcriber or partial_whisper_transcriber
    diarize_default = os.environ.get("ORPHEUS_STREAMING_DIARIZE", "").strip() in ("1", "true", "yes")
    embed_fn = embedder or ecapa_embedder
    app = FastAPI(title="Orpheus Streaming", version="0.1.0")

    @app.get("/health")
    async def health() -> dict[str, str]:
        return {"status": "ok"}

    @app.websocket("/v1/stream/transcribe")
    async def stream_transcribe(ws: WebSocket) -> None:
        await ws.accept()
        config = StreamConfig(
            turn_backend=os.environ.get("ORPHEUS_STREAMING_TURN_BACKEND", "energy"),
            diarize=diarize_default,
        )
        session: StreamSession | None = None
        redact_cfg: dict[str, Any] | None = None

        async def ensure_session() -> StreamSession:
            nonlocal session
            if session is None:
                session = StreamSession(
                    transcriber=tx,
                    partial_transcriber=partial_tx,
                    embedder=embed_fn,
                    config=config,
                    redact=redact_cfg,
                )
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
                    if isinstance(control.get("turn_backend"), str):
                        config.turn_backend = control["turn_backend"]
                    if isinstance(control.get("diarize"), bool):
                        config.diarize = control["diarize"]
                    if isinstance(control.get("turn_events"), bool):
                        config.turn_events = control["turn_events"]
                    if isinstance(control.get("redact"), dict):
                        redact_cfg = control["redact"]  # {enabled, entities, mask}
                    session = None  # reset for a fresh stream
                    await ws.send_text(json.dumps({"type": "ready"}))
                elif ctype == "bot_state":
                    # The agent tells us whether its TTS is playing, so a user
                    # speech_start while it plays raises a barge_in.
                    if isinstance(control.get("speaking"), bool):
                        config.bot_speaking = control["speaking"]
                        if session is not None:
                            session.config.bot_speaking = control["speaking"]
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

    @app.websocket("/v1/stream/converse")
    async def stream_converse(ws: WebSocket) -> None:
        """Full-duplex speech-to-speech: ASR → LLM → TTS with barge-in (PRD 15).

        Same PCM16 input as ``/v1/stream/transcribe``; in addition to transcript
        events it emits ``bot_reply`` (agent text), ``tts_audio`` (base64 wav to
        play), and ``tts_cancel`` (stop playback — the user barged in).
        """
        from .converse import ConverseSession
        from .llm import get_llm
        from .tts import get_tts

        await ws.accept()
        config = StreamConfig(
            turn_backend=os.environ.get("ORPHEUS_STREAMING_TURN_BACKEND", "energy"),
            turn_events=True,
        )
        session: ConverseSession | None = None

        def ensure() -> ConverseSession:
            nonlocal session
            if session is None:
                asr = StreamSession(
                    transcriber=tx, partial_transcriber=partial_tx,
                    embedder=embed_fn, config=config,
                )
                session = ConverseSession(asr=asr, llm=get_llm(), tts=get_tts())
            return session

        try:
            while True:
                msg = await ws.receive()
                if msg["type"] == "websocket.disconnect":
                    break
                if (data := msg.get("bytes")) is not None:
                    for ev in ensure().add_audio(data):
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
                    session = None
                    await ws.send_text(json.dumps({"type": "ready"}))
                elif ctype == "bot_state":
                    if isinstance(control.get("speaking"), bool):
                        config.bot_speaking = control["speaking"]
                        if session is not None:
                            session.asr.config.bot_speaking = control["speaking"]
                elif ctype in ("finalize", "stop", "close"):
                    for ev in ensure().finalize():
                        await ws.send_text(json.dumps(ev))
                    break
        except WebSocketDisconnect:
            pass
        finally:
            with contextlib.suppress(RuntimeError):
                await ws.close()

    @app.websocket("/v1/telephony/twilio")
    async def twilio_stream(ws: WebSocket) -> None:
        """Telephony bridge: Twilio Media Streams (μ-law 8kHz) ↔ the S2S loop.

        A carrier call terminated by Twilio streams μ-law audio here; we transcode
        to PCM16 16kHz, run the ConverseSession (ASR→LLM→TTS), and stream the bot's
        TTS back as μ-law media frames — sending a ``clear`` on barge-in so the
        caller can talk over the agent. DTMF digits are surfaced to the app.
        """
        import io as _io

        from .converse import ConverseSession
        from .llm import get_llm
        from .telephony import (
            parse_twilio_frame,
            pcm16_any_to_twilio_media,
            twilio_clear,
            twilio_media_to_pcm16_16k,
        )
        from .tts import get_tts

        await ws.accept()
        config = StreamConfig(sample_rate=16_000, turn_events=True)
        session: ConverseSession | None = None
        stream_sid: str | None = None

        async def pump(events: list[dict[str, Any]]) -> None:
            for ev in events:
                t = ev.get("type")
                if t == "tts_audio" and stream_sid:
                    with wave.open(_io.BytesIO(base64.b64decode(ev["audio_b64"])), "rb") as w:
                        sr, pcm = w.getframerate(), w.readframes(w.getnframes())
                    for fr in pcm16_any_to_twilio_media(pcm, sr, stream_sid):
                        await ws.send_text(json.dumps(fr))
                elif t in ("tts_cancel", "barge_in") and stream_sid:
                    await ws.send_text(json.dumps(twilio_clear(stream_sid)))

        try:
            while True:
                msg = await ws.receive()
                if msg["type"] == "websocket.disconnect":
                    break
                text = msg.get("text")
                if text is None:
                    continue
                try:
                    frame = json.loads(text)
                except json.JSONDecodeError:
                    continue
                p = parse_twilio_frame(frame)
                if p["kind"] == "start":
                    stream_sid = p.get("stream_sid")
                    asr = StreamSession(
                        transcriber=tx, partial_transcriber=partial_tx,
                        embedder=embed_fn, config=config,
                    )
                    session = ConverseSession(asr=asr, llm=get_llm(), tts=get_tts())
                elif p["kind"] == "media" and session is not None:
                    pcm16 = twilio_media_to_pcm16_16k(p["payload"])
                    await pump(session.add_audio(pcm16))
                elif p["kind"] == "dtmf":
                    await ws.send_text(json.dumps({"event": "dtmf", "digit": p.get("digit", "")}))
                elif p["kind"] == "stop":
                    if session is not None:
                        await pump(session.finalize())
                    break
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
