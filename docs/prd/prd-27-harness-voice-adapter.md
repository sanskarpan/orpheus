# PRD — Harness Voice Adapter (external orchestrator service)

**Status:** Proposed · **Priority:** P1 (personal) · **Implementation repo:** new, standalone (e.g.
`~/alfred/repos/alfred-voice-bridge`) — the *code* is explicitly **not** inside `orpheus`, not a
branch of `orpheus`; only this spec document is indexed here in `docs/prd/`, at Sanskar's explicit
direction, so it numbers alongside the rest of the project's PRDs. It remains scope-marked below as
personal-path infrastructure, not an Orpheus SaaS product feature, and is not subject to this
directory's multi-tenant/RLS conventions. · **Depends on (reads, does not modify):** Orpheus
`apps/workers/src/orpheus_workers/streaming.py` `/v1/stream/transcribe`, `infra/modal/orpheus_tts.py`
· **Companion PRD:** [`prd-28-streaming-tts-service.md`](prd-28-streaming-tts-service.md)

## 1. Problem

Sanskar wants to talk to his coding assistant (Claude Code / the harness) the way he'd talk to a
person — mic in, voice out, interruptible, on the always-on Oracle Cloud VM — and Orpheus already
has almost every low-level piece this needs: a tuned streaming ASR with LocalAgreement-2
incremental decoding, energy VAD, three pluggable endpoint-detection backends, and a full
turn-taking event vocabulary (`partial`, `final`, `speech_start`/`speech_end`, `barge_in`,
`backchannel`, `voicemail`, `endpoint_speculative`/`endpoint_cancel`, `endpoint`, `done`, `error`)
emitted over `/v1/stream/transcribe` (`streaming.py:854-931`). Nothing in the repo, however,
*listens* to that socket and drives a real agent with it.

The only existing thing that composes ASR into a full turn loop is `ConverseSession`
(`apps/workers/src/orpheus_workers/converse.py`), and it is confirmed unusable as the harness's
seat: `_respond()` (`converse.py:81-118`) calls `self.llm.complete(system, user, max_tokens) -> str`
— `LLMProvider.complete()` per the Protocol in `llm.py:54` — a single **synchronous, single-shot,
full-string-in/full-string-out** call, then a single blocking `self.tts.synth(reply, voice)`. There
is no token streaming, no tool-calling field anywhere in the Protocol, and history is a flattened
12-turn text blob (`_render_prompt()`, `converse.py:73-79`) rather than real multi-turn message/tool
state. Plugging Claude Code's agentic loop (tools, planning, streaming) into this exact slot would
strip it down to one blocking completion — confirmed independently in the harness-adapter-surface
research and scored dead last among integration options for exactly this reason. Widening or
duplicating `LLMProvider` to fit the harness was also evaluated and rejected: `LLMProvider` has
real existing callers (translate/summarize/detect-language, and `ConverseSession` itself), so
either change creates two divergent "what is an LLM call" patterns living in the same product
module — the saasCompat research names this explicitly as the contamination event to avoid.

So: nothing today (a) opens `/v1/stream/transcribe` as a client, (b) dispatches confirmed turns
(`endpoint`) to an agentic loop, (c) streams that loop's output into TTS at sentence boundaries
instead of waiting for the full reply, or (d) reacts to `barge_in`/`endpoint_cancel` by cancelling
the harness's own in-flight generation and audio. This is the one component that genuinely does
not exist anywhere in the repo and cannot be approximated by a config flag or a small patch — it
requires a new process with its own state machine. This PRD specifies that process.

## 2. User story

> As Sanskar, working at his desk or away from the keyboard, I want to speak a request to my
> coding assistant and hear it start responding within about a second and a half of when I stop
> talking, and I want to be able to interrupt it mid-sentence — by just talking over it — and have
> it actually stop generating and stop talking, not just go quiet on my speaker while it keeps
> "thinking" in the background. I want it to remember what we've been talking about across the
> whole conversation, and I want it to be able to actually do things (read a file, run a command,
> check a log) as part of answering me, not just describe what it would do.

## 3. Objective

Ship a standalone service that turns Orpheus's raw ASR/turn-taking WebSocket into a live,
interruptible, tool-using voice interface for Claude Code, without changing a single line of
`orpheus`. Concretely:

- Consume `/v1/stream/transcribe` as an ordinary WS client (co-located, same VM).
- On each `endpoint`, hand the turn's confirmed text to the harness's own agentic loop (streaming
  tokens, tool calls, persistent memory — native to Claude Code / Claude Agent SDK).
- Pipeline the harness's streaming reply into TTS at sentence boundaries so audio starts before
  the full reply is generated — the single biggest lever on time-to-first-audio per the latency
  budget research (§13 below).
- On `barge_in`, actually cancel the harness's in-flight step and any queued/in-flight TTS, not
  just mute local playback.
- Measure what Orpheus itself has never measured for this path: endpoint→first-TTS-byte and
  barge-in-signal→playback-stop, so the PRD-05 latency targets stop being unverified prose.

Non-goals for v1: remote/off-VM deployment (WS-over-TCP head-of-line-blocking risk makes that a
separate, harder problem — see §5 Non-Functional Requirements); multi-user/multi-tenant operation;
telephony; a learned addressee/wake-word classifier (Orpheus doesn't have one either — M3 in
PRD-05 is unstarted); replacing Modal TTS with the low-latency streaming TTS service (that's the
companion TTS PRD — this service is built to swap to it via a pluggable interface, not to build it).

## 4. Functional Requirements

**FR1 — ASR client.** Open a WebSocket to Orpheus's worker at `ws://127.0.0.1:8082/v1/stream/transcribe`
(the port `create_app()`'s uvicorn binds to per `streaming.py`'s `main()`), send a `start` control
frame (`sample_rate=16000`, `turn_events=true`, `diarize=false`, `redact=false`), then stream raw
PCM16 mono frames captured from the local mic at whatever chunk size the capture library produces
(no fixed framing required — `StreamSession.add_audio()` buffers on its own cadence).

**FR2 — Turn accumulation.** Maintain a per-turn text buffer from `final` events (never re-buffer
`partial` — per `streaming.py`'s own docstring, `final` is confirmed-once and never re-sent).
Track `turn_id` continuity so a turn's accumulated `final` text is unambiguous when `endpoint`
fires for that `turn_id`.

**FR3 — Turn dispatch.** On `endpoint`, hand the accumulated final text for that turn to the
harness as one user message. The harness process owns the full agentic loop from there: tool
calls, intermediate reasoning, and a streamed text reply — none of this flows through
`llm.py`/`LLMProvider` at any point.

**FR4 — Speculative pre-warm (optional, off by default in v1).** On `endpoint_speculative`, the
adapter *may* pre-issue a lightweight "get ready" signal to the harness (e.g., opening the
turn/context without committing a user message) so a true `endpoint` shortly after has less setup
latency. On `endpoint_cancel`, retract/discard that speculative state with no side effects. Because
speculative dispatch risks the harness starting real work (tool calls with side effects) on a turn
that gets retracted, v1 ships this **disabled by default**, gated by a config flag, until the
harness-side "preview" mode is proven side-effect-free.

**FR5 — Sentence-boundary TTS pipelining.** As the harness streams its reply, buffer tokens and
flush a synthesis request as soon as a sentence-terminal boundary is detected (`.`, `!`, `?`,
newline, or a max-buffer timeout of ~2s to avoid stalling on a long clause with no punctuation).
Each flushed sentence is synthesized and queued for playback independently and in order; playback
of sentence *N* may begin while sentence *N+1* is still being synthesized or the harness is still
generating sentence *N+2*. The adapter must never wait for the harness's full reply before issuing
the first synthesis call.

**FR6 — Barge-in cancellation.** On `barge_in`, in this order: (a) send a cancel signal to the
harness process for its current turn (stop token generation and abort any in-flight tool call the
harness supports aborting), (b) discard any TTS requests still queued/in-flight for the interrupted
turn, (c) stop local audio playback immediately, (d) if a TTS backend exposes a synthesis-cancel
RPC (the interim Modal backend does not — see §6), call it; otherwise let the in-flight synthesis
complete and simply discard its output. Emit a new turn boundary so the interrupting speech becomes
the start of the next turn.

**FR7 — Session/turn state ownership.** The adapter (and the harness process it drives) owns all
conversational memory. It must not read or depend on `ConverseSession._history` — that object is
never instantiated, because the adapter never opens `/v1/stream/converse`. Memory persistence
across process restarts is out of scope for v1 (Claude Code's own session/transcript mechanism is
the source of truth; the adapter does not duplicate it).

**FR8 — TTS backend interface.** Define a small internal `SynthesisBackend` interface —
`synth(text: str, voice: str | None) -> AudioChunk` at minimum, plus an optional `cancel(request_id)`
— with exactly one implementation shipped in v1 (`ModalKokoroBackend`, §6) and the interface
designed so the eventual streaming-TTS service (the companion TTS PRD) can be swapped in via
config with no changes to FR5/FR6's orchestration logic.

**FR9 — Bypass Orpheus's Go/session tier.** No call ever goes through `POST /v1/streaming/sessions`,
no `X-API-Key`, no HMAC stream token minted by `MintStreamToken`. The adapter talks to the Python
worker's WebSocket directly and to Modal's TTS endpoint directly. This is a deliberate architectural
choice (§9, §14) — not an oversight.

**FR10 — Timing instrumentation.** Record, per turn: `t_endpoint` (endpoint event received),
`t_first_llm_token` (harness's first streamed token), `t_first_tts_request`,
`t_first_tts_byte` (first audio byte back from synthesis), `t_first_playback` (first sample played
locally), and — on barge-in turns — `t_barge_in_signal` and `t_playback_stopped`. Compute and log
`endpoint_to_first_tts_byte_ms` and `barge_in_to_stop_ms` per turn (§13, §16).

## 5. Non-Functional Requirements

- **Co-located deployment only (v1).** The adapter and the Orpheus worker must run on the same VM
  (loopback WS) or an equivalently low-jitter path (e.g. same LAN). Orpheus's own design doc
  (`docs/design/12-streaming-realtime.md`) frames WebRTC as the answer to WAN-path jitter/loss;
  this adapter takes the same position the harness-adapter research did — WS-over-TCP is fine when
  both ends are effectively local, and is explicitly out of scope to harden for a remote/lossy path
  in v1. Document this constraint in the README; do not silently degrade if deployed remotely.
- **Single active conversation.** v1 supports exactly one live mic session at a time — no
  multi-session concurrency, no session pooling. This matches the single-user, first-party use case
  and avoids building concurrency-cap machinery Orpheus's own M5 milestone (PRD-05 §5) hasn't
  finished either.
- **Resilience to Orpheus worker restarts.** If the `/v1/stream/transcribe` WebSocket drops, the
  adapter must detect the close, log it, attempt a bounded reconnect (e.g. 3 attempts with backoff),
  and if reconnect fails, surface a clear local error state (not a silent hang) — see §12.
  In-flight harness turns are not resumed across a drop; the current turn is abandoned cleanly.
  Note: `t_endpoint` and other timers reset per-turn, so a mid-turn drop does not corrupt the
  next turn's latency measurement.
- **No new load on Orpheus's product infra.** The adapter must not create `streaming_sessions` rows,
  must not consume org-scoped rate limits, and must not appear in Orpheus's billing/metering
  pipeline — it is invisible to the product's multi-tenant accounting by design (FR9).
- **Config, not code, for tunables.** Sentence-boundary punctuation set, max-buffer timeout,
  reconnect attempts/backoff, and the active `SynthesisBackend` must be config values (env vars or
  a small YAML/TOML file), not hardcoded, so behavior can be tuned without a redeploy.

## 6. Technical Requirements

- **Language/runtime:** Python 3.11+ (matches Orpheus's own worker stack, eases reuse of the
  `websockets`/`httpx` idioms already proven in `modal_client.py`), running as its own long-lived
  process (systemd unit or a simple supervised script — not inside any Orpheus container).
- **ASR client:** a WebSocket client (`websockets` or `httpx`-ws) implementing FR1/FR2, structured
  as an async event loop parsing each JSON/binary frame per the exact schema documented in the
  harness-adapter-surface research (`ready`, `partial`, `final`, `speaker_update`, `speech_start`,
  `speech_end`, `barge_in`, `backchannel`, `voicemail`, `endpoint_speculative`, `endpoint_cancel`,
  `endpoint`, `done`, `error`) — no schema translation needed, consume as-is.
- **Harness driver:** a thin wrapper around Claude Code / Claude Agent SDK invoked per turn, using
  its native streaming-token and tool-calling support (this is the whole point of not routing
  through `llm.py`). The wrapper's only job is to (a) submit the turn's text + prior turn context,
  (b) surface a token/sentence stream back to the TTS pipeliner (FR5), and (c) expose a cancel
  handle for FR6. Exact SDK wiring is an implementation detail deferred to build time, not
  specified further here — this PRD constrains the *contract* (streaming in, cancellable, tool-using),
  not the SDK call shape.
- **Interim TTS backend — `ModalKokoroBackend`:** calls `infra/modal/orpheus_tts.py`'s existing
  Kokoro endpoint directly, POSTing `{"token": ORPHEUS_MODAL_SHARED_SECRET, "text", "voice?",
  "speed?"}` and reading back `{audio_b64 (24kHz mono WAV PCM16), sample_rate, duration_seconds,
  ...}` — the exact contract confirmed in the harness-adapter-surface research. `ORPHEUS_MODAL_SHARED_SECRET`
  is read from `~/.config/alfred/env` (chmod 600) per this VM's existing secrets convention — never
  committed, never logged. This backend has **no cancel primitive** (§13, §16 Failure Scenarios) —
  `cancel()` on `ModalKokoroBackend` is a no-op that only stops *consuming* the response, consistent
  with FR6(d).
- **Audio I/O:** local mic capture and speaker playback via a standard cross-platform audio library
  (e.g. `sounddevice`/`pyaudio`); playback must support hard-stop-and-flush for barge-in (FR6c),
  not just pause.
- **Packaging:** own `pyproject.toml`, own virtualenv, own README documenting the co-location
  requirement (§5) and the required env vars (`ORPHEUS_STREAM_WS_URL`, `ORPHEUS_MODAL_SHARED_SECRET`,
  `ORPHEUS_MODAL_TTS_URL`, harness invocation config).

## 7. API Requirements

This service is a **client**, not a server, for its Orpheus-facing side — it exposes no public API
of its own in v1 beyond an optional local debug endpoint (below). Its "API surface" is the contract
it consumes and produces:

**Consumes — Orpheus `/v1/stream/transcribe` (unmodified):**
- Outbound control frames: `{"type": "start", "sample_rate": 16000, "turn_events": true}`,
  `{"type": "finalize"}`, `{"type": "stop"}`, `{"type": "close"}`, plus optional `{"type":
  "bot_state", "bot_speaking": bool}` if the adapter chooses to report its own playback state back
  into the ASR session for `barge_in` computation (`streaming.py`'s `_detect_speech_events` needs
  `bot_speaking=true` set by *something* — the adapter must send this frame whenever it starts/stops
  local TTS playback, since there is no `ConverseSession` doing it automatically on this path).
- Outbound binary frames: raw PCM16 mono audio chunks.
- Inbound JSON events: the full event set in §6, consumed as-is.

**Consumes — Modal `orpheus-tts` endpoint (unmodified):** `POST {ORPHEUS_MODAL_TTS_URL}` with the
shared-secret payload above; response per §6.

**Produces (optional, v1 nice-to-have):** a local-only `GET /debug/status` (loopback-bound, no
auth beyond bind address) returning current turn state, last-turn timing (§4 FR10), and connection
health — for Sanskar to `curl` while debugging, not a product API.

## 8. Architecture

```
                         Sanskar's mic / speaker
                                  |
                                  v
        +---------------------------------------------------+
        |     alfred-voice-bridge  (this service, new repo)  |
        |                                                     |
        |   +-----------+   final/endpoint  +--------------+  |
        |   | ASR client|------------------>| Turn dispatch|  |
        |   | (WS)      |<------------------| + state      |  |
        |   +-----+-----+   bot_state frame +------+-------+  |
        |         ^                                 |          |
        |         | PCM16 frames            turn text (user)   |
        |         |                                 v          |
        |   +-----+-----+                    +--------------+  |
        |   | Mic       |                    | Harness      |  |
        |   | capture   |                    | driver       |  |
        |   +-----------+                    | (Claude Code |  |
        |                                     |  / Agent SDK)|  |
        |   +-----------+   sentence chunks   +------+-------+  |
        |   | Speaker   |<--------------------------+           |
        |   | playback  |        streamed reply tokens          |
        |   | (cancel-  |                     |                 |
        |   |  able)    |                     v                 |
        |   +-----+-----+              +--------------+         |
        |         ^                    | Sentence     |         |
        |         | audio chunks       | segmenter +  |         |
        |         +--------------------| TTS pipeline |         |
        |                              | (Synthesis-  |         |
        |                              |  Backend)    |         |
        |                              +------+-------+         |
        |                                     |                  |
        +-------------------------------------|------------------+
                                               v
                          +---------------------------------------+
                          |  ModalKokoroBackend (interim, v1)      |
                          |  -> infra/modal/orpheus_tts.py         |
                          |     (Kokoro, shared-secret POST,       |
                          |      whole-utterance, no cancel RPC)   |
                          +---------------------------------------+

        ------------------------------------------------------------
        Orpheus (unmodified — zero lines changed):
          apps/workers/src/orpheus_workers/streaming.py
            -> FastAPI app, /v1/stream/transcribe (WS, consumed above)
          infra/modal/orpheus_tts.py
            -> Modal Kokoro TTS endpoint (POST, consumed above)
        Explicitly bypassed (never called by this service):
          apps/workers/src/orpheus_workers/converse.py (ConverseSession)
          apps/workers/src/orpheus_workers/llm.py (LLMProvider)
          apps/api Go relay + streaming_sessions REST/RLS/billing tier
        ------------------------------------------------------------
```

## 9. Data Flow

1. Adapter starts, opens `/v1/stream/transcribe`, sends `start`, begins streaming mic PCM16.
2. Orpheus's `StreamSession` emits `partial` events continuously (adapter logs/discards — not
   used for dispatch) and `final` events as words are confirmed (LocalAgreement-2); adapter appends
   each `final`'s text to the current turn's buffer, keyed by `turn_id`.
3. Orpheus's endpoint detector (default `energy` backend per `StreamConfig.turn_backend`) fires
   `endpoint` after `vad_silence_seconds` (0.6s default) of trailing silence; adapter reads
   `t_endpoint = now()`, takes the buffered text for that `turn_id`, and calls the harness driver
   with it as a new user turn.
4. Harness driver streams tokens/sentences back; on each sentence boundary (FR5), the adapter marks
   `t_first_llm_token` (first call only) and hands the sentence to the TTS pipeline.
5. TTS pipeline calls `ModalKokoroBackend.synth(sentence, voice)`; on first response for the turn,
   adapter records `t_first_tts_byte`; decoded PCM is queued to the playback device, and adapter
   sends `{"type":"bot_state","bot_speaking":true}` to the ASR socket so Orpheus's own VAD can
   compute `barge_in` correctly against playback state.
6. Playback starts; adapter records `t_first_playback`. Subsequent sentences continue streaming in
   from the harness and queuing to TTS/playback in order, overlapping harness generation with
   synthesis and playback of earlier sentences.
7. If the user starts speaking while `bot_speaking=true`, Orpheus's `StreamSession` emits `barge_in`
   on the ASR socket (`_detect_speech_events`, `streaming.py:539-555`). Adapter executes FR6: cancel
   harness step, drop queued TTS, hard-stop playback, send `bot_state: false`. The interrupting
   speech starts accumulating as the next turn's `final` text under a new `turn_id`.
8. On normal completion (harness reply fully spoken, no barge-in), adapter sends `bot_state: false`
   and returns to listening for the next `endpoint`.
9. Turn timing (FR10) is logged/emitted at the end of every turn regardless of whether it was
   interrupted.

## 10. Dependencies

- **Orpheus worker process** running and reachable at the configured WS URL, with
  `/v1/stream/transcribe` mounted (`create_app()` in `streaming.py` — no feature flag gates this
  route, unlike `/v1/telephony/twilio` which requires `ORPHEUS_TELEPHONY_ENABLED`).
- **`ORPHEUS_MODAL_SHARED_SECRET`** and a reachable `orpheus-tts` Modal deployment
  (`infra/modal/orpheus_tts.py`, `modal deploy`), or the interim backend has nothing to call.
  Per CLAUDE.md, a Modal token will be provided later — this dependency is **not yet satisfied**
  as of this PRD and blocks any TTS output until it is (ASR-only operation, i.e. transcript
  without spoken replies, is possible for early testing without it).
- **Claude Code / Claude Agent SDK**, runnable as a local process on this VM under Sanskar's
  subscription (per CLAUDE.md's Models section) — this is the harness driver dependency.
- **Local audio I/O libraries** (mic capture + cancellable playback) as an OS-level dependency on
  the Oracle Cloud ARM VM (Ubuntu 24.04) — needs verification that ALSA/PulseAudio (or an
  equivalent) is available and configured on a VM that may not have audio hardware by default;
  if the VM has no audio device, this adapter's mic/speaker legs need to run on Sanskar's local
  machine instead, with only the ASR/TTS network legs reaching the VM — **this is an open
  deployment question, not resolved by this PRD** (see §17 Rollout Strategy, Phase 0).

## 11. GPU Requirements

This service itself requires **no GPU** — it is a thin orchestration process (WS client, text
buffering, HTTP calls, audio I/O). All GPU work happens inside Orpheus's existing infra:
- ASR: `StreamSession`'s transcriber, which defaults to local CPU faster-whisper (`tiny.en`) unless
  `ORPHEUS_WORKER_TRANSCRIBE_BACKEND=modal` is set on the Orpheus worker — this adapter has no
  control over that choice, it inherits whatever the worker is configured to run.
- TTS: Modal's `orpheus-tts` app, `gpu="a10g"`, `min_containers=0`, `scaledown_window=300`
  (`infra/modal/orpheus_tts.py`) — scale-to-zero, so any request after a 5-minute gap pays a full
  cold start. Per the Modal-suitability research, this is a known, accepted latency risk for v1
  (see §13), not something this PRD attempts to fix by requesting `min_containers=1` — the prior
  research is explicit that warming Modal for one personal user costs ~$790–2400/month for GPU
  time that sits idle almost all day, which is irrational for this use case. If sub-second,
  cold-start-free TTS is required later, that is the companion TTS PRD's job (a dedicated
  always-on process outside Modal's serverless model), not a config change here.

## 12. Latency Requirements

Cited directly from the latency-mechanics research (grounding JSON, `latencyMechanics`) and the
cross-framework benchmark research (`externalPatterns`), since Orpheus itself has never measured
this path:

- **Floor imposed by Orpheus's own ASR/VAD before this adapter even sees an `endpoint`:**
  `vad_silence_seconds=0.6` (trailing silence required) plus up to the remainder of the
  `min_chunk_seconds=1.0` commit cadence before the confirming `final`/`endpoint` fires. This
  adapter cannot reduce this floor — it is entirely inside `StreamSession` and out of scope to
  change (§1). Budget accordingly: **~0.6–1.0s is spent before `t_endpoint` is even reachable**,
  on top of whatever this adapter adds afterward.
- **PRD-05's own target** (`docs/prd/prd-05-voice-agent.md` §6): endpoint→first-TTS-byte
  **< 1.5s** with warm Modal LLM+TTS, and barge-in event→TTS-stop **< 200ms** — both confirmed by
  the latencyMechanics research to be **unmeasured anywhere in the existing codebase** (no
  `time.monotonic()`/`perf_counter()` instrumentation around the ASR→LLM→TTS path, no latency
  test in `test_converse.py`/`test_streaming.py`/`streaming_ws_test.go`). This adapter is the
  first thing in the whole stack to actually measure it (FR10).
- **Modal cold start risk, explicitly not masked.** `modal_client.py`'s default timeout is 600s
  and follows the 303 long-poll chain transparently — a cold `orpheus-tts` container can make the
  interim TTS backend's first call in a session take many seconds, not milliseconds. Orpheus's own
  PRD-05 design calls for a filler/earcon during this gap; **no such filler exists anywhere in the
  Orpheus codebase**, and this adapter does not build one for v1 either — cold TTS calls will
  produce an audible dead-air gap on the first turn of a session (or any turn after a 5-minute
  gap). This is called out explicitly as accepted v1 risk, not silently swallowed (§16).
- **Why sentence-boundary pipelining (FR5) is the adapter's one real latency lever.** Per the
  cross-framework research, every mature framework studied (Pipecat, LiveKit Agents, Vocode) starts
  TTS on the first sentence while the LLM is still generating the rest — this is what separates
  "well-tuned streaming pipeline" latency (Pipecat/LiveKit's own cited 400–800ms full-turn budgets)
  from "naive blocking pipeline" latency (1000–2000ms+, which is what `ConverseSession`'s
  serial `llm.complete()` → `tts.synth()` chain produces, per the confirmed reading of
  `converse.py:81-118`). Because this adapter routes around `ConverseSession` entirely and the
  harness natively streams tokens, FR5 is achievable in a way it structurally is not on the
  `/v1/stream/converse` seam.
- **Realistic v1 latency expectation, stated honestly.** Given the ASR floor (~0.6–1.0s), harness
  first-token latency (depends on the model/tool use, not controlled by this PRD), and a cold or
  warm Modal TTS call, **this adapter should not be expected to reliably hit PRD-05's 1.5s target
  in v1**, especially on cold-TTS turns. The acceptance criteria (§18) require *measuring and
  reporting* the real number, not asserting the target is met — closing the gap to 1.5s is likely
  to require the companion TTS PRD's low-latency backend, not further adapter-side optimization.

## 13. Failure Scenarios

| Scenario | Behavior |
|---|---|
| Orpheus worker WS unreachable at startup | Adapter logs a clear error, retries with backoff (bounded, per §5), exits non-zero after exhausting retries rather than hanging silently. |
| Orpheus WS drops mid-conversation | Detected via WS close/error; current turn is abandoned (no partial harness dispatch), bounded reconnect attempted, and on success the adapter re-sends `start` and resumes listening for the next turn. On exhausted retries, adapter surfaces a local error state and stops trying until manually restarted. |
| Modal TTS call fails or times out | Per FR8, `ModalKokoroBackend.synth()` raises; adapter logs the failure with the turn's timing data (marking `t_first_tts_byte` as failed, not silently omitted), skips playback for that sentence, and continues with the next sentence if the harness produced more — the conversation does not crash, but that sentence is silently dropped from audio (a real, accepted UX gap for v1, since there is no local Piper-class fallback voice built into this adapter or into Orpheus's `tts.py` — Orpheus's own fallback there is `StubTTS`, a *silent* placeholder, not real fallback speech). |
| Modal TTS cold start (5+ min idle) | No special handling — the call simply takes longer (up to `modal_client.py`'s 600s ceiling in the worst case); this shows up as a large `endpoint_to_first_tts_byte_ms` in the timing log (§16), which is the intended signal to notice the problem, not silently absorb it. |
| Harness process crashes or hangs mid-turn | Adapter must apply a bounded per-turn timeout on the harness call (config value, not hardcoded); on timeout, cancel the harness call if the SDK supports it, log the failure, and speak (or silently skip, per config) a short fallback utterance rather than hanging the whole session waiting for a turn that will never complete — mirroring the `llm_turn_timeout`/fallback-utterance requirement PRD-05 itself specifies but never implemented in `converse.py`. |
| `barge_in` fires but harness has no cancel handle for the in-flight step | Best-effort: stop consuming/playing the harness's further output and any queued TTS (FR6b/c) even if the underlying harness call cannot be aborted server-side; log that true cancellation was not achieved so this gap is visible in the timing data, not silently masked as a clean cancel. |
| No audio hardware available on the VM (open question, §10) | Adapter must fail fast at startup with a clear message identifying the missing device, not fall back to a degraded silent mode that looks like it's working. |
| Malformed/garbage bytes from mic capture | Adapter validates frame sizes before forwarding to the ASR WS; on a malformed frame, drop it and log, do not forward corrupt bytes that could destabilize `StreamSession`'s buffer assumptions. |

## 14. Security Considerations

- **`ORPHEUS_MODAL_SHARED_SECRET` handling.** Read from `~/.config/alfred/env` (chmod 600) per
  this VM's existing secrets convention (CLAUDE.md). Never logged, never included in the debug
  status endpoint (§7), never committed to the adapter's own repo (add it to `.gitignore` by
  pattern, not by relying on discipline alone — e.g. never read raw `.env` files into a committed
  fixture or example).
- **No Orpheus multi-tenant credentials involved at all.** By design (FR9), this adapter never
  touches `X-API-Key`, org auth, or HMAC stream tokens — there is no tenant-isolation surface for
  it to violate, because it never enters that surface. This is a deliberate simplification, not an
  oversight: per the saasCompat research, the actual contamination risk to Orpheus's product would
  be adding a "local mode"/no-auth bypass *inside* Orpheus's own auth code — this adapter avoids
  that risk entirely by never asking Orpheus's Go tier for anything.
- **Loopback-only exposure.** The optional debug endpoint (§7) must bind to `127.0.0.1` only, never
  `0.0.0.0` — it carries turn text and timing, not secrets, but should not be reachable off-VM.
- **Mic audio handling.** Raw PCM is sent to the local Orpheus worker (same VM) and never leaves
  the machine except as text turns to the harness and synthesis requests (text, not audio) to
  Modal — no raw audio is sent to any third-party service in this design. Audio is not persisted
  to disk by this adapter (no recording feature in v1); if debug logging of audio is added later,
  it must be opt-in and clearly flagged given it captures Sanskar's actual speech.
- **Harness tool-calling blast radius.** Because the harness runs with real tool-calling on this
  VM (per CLAUDE.md, e.g. it can run commands, edit files), voice-triggered turns carry the same
  authority as any other Claude Code session on this machine — this adapter does not add new
  permission scoping of its own. This is accepted as consistent with "Sanskar talking to his own
  assistant" but should be revisited if this adapter is ever extended beyond single-user use.

## 15. Testing Strategy

- **Unit tests (no network, no audio hardware).** ASR event parser: feed synthetic JSON frames
  matching the exact schema from `streaming.py`, assert correct turn-buffer accumulation on
  `final`, correct dispatch on `endpoint`, correct discard on a stray `endpoint` with an empty
  buffer. Sentence segmenter: feed a token stream with punctuation, mid-word boundaries, and a
  long unpunctuated run past the max-buffer timeout; assert correct flush points. Turn-timing
  recorder: assert all six FR10 timestamps are captured in a synthetic end-to-end fake turn and
  that barge-in turns additionally capture the two barge-in timestamps.
- **Integration tests against a real (local) Orpheus worker.** Spin up `orpheus_workers`'s
  `create_app()` locally (as `test_streaming.py`/`test_converse.py` already do), point the adapter
  at it, feed a synthetic PCM wav of known speech, and assert: a `final`→`endpoint` sequence
  reaches the turn-dispatch stage with the correct accumulated text; a `barge_in` event injected
  mid-turn triggers FR6's cancellation path (assert the harness-cancel call and playback-stop are
  both invoked, using a fake harness driver and fake playback device for this test).
- **Harness driver contract test.** Against a stubbed/fake harness driver (not live Claude Code, to
  keep this test fast/deterministic), assert the adapter correctly streams sentence chunks to the
  TTS pipeline as they arrive rather than buffering the full reply — this is FR5's core guarantee
  and the single most important behavioral test in the suite, directly verifying the latency
  argument in §13.
- **TTS backend contract test.** Against a fake `SynthesisBackend`, assert `ModalKokoroBackend`'s
  request/response shape matches the documented `orpheus_tts.py` contract (shared-secret field
  name, response field names) — a schema drift here fails loudly rather than as a silent runtime
  500 the first time it's actually used.
- **Manual/live end-to-end acceptance test (required, cannot be fully automated).** With a real
  Orpheus worker, a real (warm, pre-request-primed) Modal TTS deployment, and a real harness
  process, run a live conversation: speak a request, measure wall-clock from when speech stops to
  when audio starts (§16); speak over the reply mid-sentence and confirm the adapter stops talking
  audibly (not just that logs claim it did) within a perceptible instant. This is the test that
  actually validates the product experience the PRD exists to deliver.

## 16. Observability

- **Structured per-turn log line** (not metrics-backend-dependent for v1 — plain structured JSON
  to stdout/a log file is sufficient given single-user scale) containing: `turn_id`,
  `t_endpoint`, `t_first_llm_token`, `t_first_tts_request`, `t_first_tts_byte`, `t_first_playback`,
  `endpoint_to_first_tts_byte_ms`, whether the turn was interrupted, and if so
  `t_barge_in_signal`/`t_playback_stopped`/`barge_in_to_stop_ms`.
- **Session-level summary on adapter exit or on request** (via the debug endpoint, §7): p50/p95 of
  `endpoint_to_first_tts_byte_ms` and `barge_in_to_stop_ms` across the session's turns so far — the
  first place these numbers exist anywhere in the Orpheus ecosystem, per the latencyMechanics
  research's confirmed finding that no such instrumentation exists today.
- **Connection health.** Log WS connect/disconnect/reconnect events for the Orpheus ASR socket,
  and log every TTS call's success/failure/duration distinctly from the turn-timing log so a string
  of Modal cold starts is visible as a pattern, not just individually slow turns.
- **No PII/audio in logs.** Turn text (transcripts) may be logged for debugging since this is a
  single-user personal tool, but this should be a config-gated verbosity level (default: log
  lengths/hashes, not raw text) so logs aren't a silent transcript archive by accident.

## 17. Rollout Strategy

- **Phase 0 — feasibility spike (before writing the full adapter).** Confirm audio I/O actually
  works on the target VM (the open question in §10) — if the Oracle Cloud ARM VM has no usable
  audio device, decide now whether mic/speaker legs run on Sanskar's local machine with only
  network legs reaching the VM, since that changes the architecture diagram (§8) materially.
- **Phase 1 — ASR-only smoke test.** Build FR1–FR3 only (WS client, turn accumulation, dispatch)
  with the harness driver stubbed to just print the turn text — validates the Orpheus-facing half
  of the adapter without needing Modal TTS access yet (per §10, the Modal token isn't provisioned
  as of this PRD).
  - Then extend FR3's stub into the real harness driver: dispatch to a live Claude Code session,
    print its streamed reply to the terminal (no TTS yet) — validates FR3's contract end-to-end
    on the text side.
- **Phase 2 — TTS pipeline, once Modal is provisioned.** Build FR5/FR8 (`ModalKokoroBackend`),
  wire sentence-boundary flushing, get first spoken audio out. Measure and log real
  `endpoint_to_first_tts_byte_ms` numbers for the first time (FR10) — expect this to be materially
  above 1.5s on cold-Modal turns, per §13's honest expectation; that's expected data, not a bug.
- **Phase 3 — barge-in.** Build FR6 end-to-end (harness cancel + TTS discard + playback hard-stop),
  validate with the manual live test in §15, measure `barge_in_to_stop_ms`.
- **Phase 4 — daily-use hardening.** Reconnect logic (§5), failure-scenario handling (§16), debug
  endpoint, and enough polish that Sanskar can leave it running and actually use it day to day.
- No staged rollout to other users is applicable — this is a single-user tool. "Rollout" here means
  incremental capability build-out on one machine, not a deployment ring.

## 18. Acceptance Criteria

1. **Functional turn-taking correctness.** A live conversation of at least 5 back-and-forth turns
   completes with each user utterance correctly triggering exactly one harness dispatch (no
   duplicate dispatch on retried `endpoint`s, no dropped turns), verified by turn-by-turn log
   inspection (§16).
2. **Sentence-boundary pipelining is real, not simulated.** For at least one multi-sentence harness
   reply, log evidence shows `t_first_tts_request` for sentence 1 occurring before the harness's
   full reply has finished streaming (i.e., synthesis started while generation was still ongoing) —
   directly falsifiable from the per-turn timing log, not just claimed.
3. **Barge-in produces audible, not just logical, cancellation.** Speaking over an in-progress
   reply during the manual live test (§15) audibly stops playback, confirmed by a human listening,
   with `barge_in_to_stop_ms` logged and under 1 second even though the interim TTS backend has no
   cancel RPC (playback hard-stop alone should achieve this; generation-cancel may lag).
4. **Measured, reported end-to-end latency — the headline number this PRD exists to produce.**
   Report p50 and p95 of `endpoint_to_first_tts_byte_ms` across at least 20 real turns (mix of
   warm and cold Modal TTS states, explicitly labeled which is which), and state plainly whether
   PRD-05's 1.5s target is met, and if not, by how much and why (ASR floor vs. harness latency vs.
   Modal cold start) — per §13, meeting the target is not required for acceptance, but an honest,
   broken-down measurement is.
5. **Zero modifications to Orpheus.** `git diff` (or equivalent) against the `orpheus` repo shows
   no changes to `streaming.py`, `converse.py`, `llm.py`, `tts.py`, `infra/modal/orpheus_tts.py`,
   or any Go relay/RLS file, at any point during this adapter's development — verified by the
   adapter living in its own repo with no vendored/patched copy of Orpheus source.
6. **Failure scenarios degrade, not crash.** Each row of §16's failure table is exercised at least
   once (e.g. by killing the Orpheus worker mid-session, or pointing `ORPHEUS_MODAL_TTS_URL` at a
   bad endpoint) and the adapter is shown to log the failure and continue or exit cleanly per the
   documented behavior, never hang indefinitely or crash with an unhandled exception.
7. **Session survives a worker reconnect.** Killing and restarting the Orpheus worker process
   mid-session (without killing the adapter) results in a logged reconnect and resumed listening
   for the next turn, per §5/§16 — the current in-flight turn may be lost, but the session as a
   whole does not require an adapter restart.
