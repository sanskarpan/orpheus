"""Full-duplex speech-to-speech loop (PRD 05 §4, #356).

``ConverseSession`` composes the streaming **ASR** (:class:`StreamSession` with
turn-taking events) → **LLM** (``llm.get_llm``) → **TTS** (``tts.get_tts``) into a
voice-agent turn loop, transport-free so it is unit-testable:

- feed audio with :meth:`add_audio`; it returns the ASR events plus, when a user
  turn ends (a firm ``endpoint``), a ``bot_reply`` (LLM text) and ``tts_audio``
  (synthesized speech) for the agent to play;
- a user ``barge_in`` (talking over the agent's TTS) emits a ``tts_cancel`` so the
  transport stops the in-flight bot audio immediately.

The FastAPI ``/v1/stream/converse`` WebSocket route in ``streaming.create_app``
drives this over the wire, reusing the same PCM16 pipeline the browser uses.
"""

from __future__ import annotations

import base64
from dataclasses import dataclass, field
from typing import Any

from .streaming import StreamSession

_DEFAULT_SYSTEM = (
    "You are a helpful voice assistant. Reply in one or two short, natural "
    "sentences suitable for being spoken aloud. Do not use markdown or lists."
)


@dataclass
class ConverseSession:
    asr: StreamSession
    llm: Any  # llm.get_llm()
    tts: Any  # tts.get_tts()
    voice: str | None = None
    system_prompt: str = _DEFAULT_SYSTEM
    max_reply_tokens: int = 120
    _history: list[tuple[str, str]] = field(default_factory=list)
    _pending: list[str] = field(default_factory=list)

    def add_audio(self, pcm: bytes) -> list[dict[str, Any]]:
        out: list[dict[str, Any]] = []
        for ev in self.asr.add_audio(pcm):
            out.append(ev)
            t = ev.get("type")
            if t == "final":
                self._pending.append(ev.get("text", ""))
            elif t == "barge_in":
                out.append({"type": "tts_cancel", "turn_id": ev.get("turn_id")})
            elif t == "endpoint":
                # Use the ENDING turn's id (the ASR has already incremented its
                # counter by the time this event is processed) so the bot_reply /
                # tts_audio correlate to the user turn they answer.
                out.extend(self._respond(ev.get("turn_id")))
        return out

    def finalize(self) -> list[dict[str, Any]]:
        out: list[dict[str, Any]] = []
        done: dict[str, Any] | None = None
        for ev in self.asr.finalize():
            if ev.get("type") == "done":
                done = ev
                continue
            out.append(ev)
            if ev.get("type") == "final":
                self._pending.append(ev.get("text", ""))
        out.extend(self._respond())  # answer any trailing user turn
        if done is not None:
            out.append(done)
        return out

    def _render_prompt(self) -> str:
        lines = []
        for role, content in self._history[-12:]:
            speaker = "User" if role == "user" else "Assistant"
            lines.append(f"{speaker}: {content}")
        lines.append("Assistant:")
        return "\n".join(lines)

    def _respond(self, turn_id: int | None = None) -> list[dict[str, Any]]:
        user_text = " ".join(t for t in self._pending if t).strip()
        self._pending = []
        if not user_text:
            return []
        # Prefer the ending turn's id (passed from the endpoint event); fall back
        # to the ASR's current counter (finalize path, where no increment raced).
        if turn_id is None:
            turn_id = self.asr._turn_id
        self._history.append(("user", user_text))
        try:
            reply = self.llm.complete(
                self.system_prompt, self._render_prompt(), max_tokens=self.max_reply_tokens
            ).strip()
        except Exception:
            reply = ""
        if not reply:
            # LLM unavailable → surface an empty bot turn rather than hanging.
            return [{"type": "bot_reply", "text": "", "turn_id": turn_id,
                     "warnings": ["llm_unavailable"]}]
        self._history.append(("assistant", reply))
        events: list[dict[str, Any]] = [
            {"type": "bot_reply", "text": reply, "turn_id": turn_id}
        ]
        try:
            audio, sr = self.tts.synth(reply, self.voice)
            if audio:
                events.append(
                    {
                        "type": "tts_audio",
                        "audio_b64": base64.b64encode(audio).decode(),
                        "sample_rate": sr,
                        "turn_id": turn_id,
                    }
                )
        except Exception:
            events[0].setdefault("warnings", []).append("tts_unavailable")
        return events
