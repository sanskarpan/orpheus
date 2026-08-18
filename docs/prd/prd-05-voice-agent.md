# PRD: Voice-Agent / Conversational Infrastructure

**Status:** Proposed · **Priority:** P1 · **Epic:** Voice Agent · **Related issues:** #351, #352, #353, #354, #355, #356

## 1. Summary

Turn Orpheus's realtime ASR into a full conversational-voice substrate: the primitives a
LiveKit/Pipecat-style voice agent needs but that live below the agent framework. This is a
**complete, production-grade** build — not an MVP or a reduced-scope prototype. The streaming
engine and Go relay are upgraded to emit **turn-taking events** — barge-in / interruption (#351),
backchannel detection (#352), active-listening / addressed-only gating (#353), and voicemail
detection (#354) — over the existing WebSocket. The build also delivers **telephony ingestion**
(SIP/RTP → WebRTC, DTMF, call transfer, #355) and a **full-duplex speech-to-speech** loop (#356)
that wires ASR → LLM → TTS behind the relay. The full feature set is in scope; the milestones in
§5 are ordering only, and each milestone is itself production-quality and independently shippable
with its own error handling, scale limits, tenant isolation, observability, and cost metering.
These are platform seams other agent frameworks integrate against, not an end-user agent product.

## 2. Motivation & goals

Orpheus already streams `partial`/`final`/`done` transcription events (`streaming.py:8`). A voice
agent needs more: it must know *when the user started talking over the bot* (barge-in), *when a
sound is just "mm-hm"* (backchannel, don't interrupt), *whether the user is addressing the agent
at all* (active listening), and *whether it reached a human or voicemail*. Today none of these
exist, so any agent built on Orpheus has to reimplement turn-taking. Shipping these as first-class
events makes Orpheus the transport + VAD + turn-detection layer that Pipecat/LiveKit-style
orchestrators sit on.

Goals:
- Emit structured turn-taking events (`speech_start`, `speech_end`, `barge_in`, `backchannel`,
  `endpoint`, `addressed`, `voicemail`) on the existing streaming WebSocket, alongside transcript
  events, without breaking current clients.
- Telephony ingestion: accept SIP/RTP and WebRTC audio, DTMF digits, and call transfer, feeding
  the same PCM16 pipeline the browser uses today.
- A full-duplex speech-to-speech path composing ASR (`streaming.py`) → LLM (`llm.py`) → TTS,
  with barge-in cancelling in-flight TTS.
- Reuse the existing session/auth/billing control plane (`streaming.go`, `streaming_ws.go`).
- Ship every one of the above with production-grade failure handling, bounded scale/concurrency,
  multi-tenant isolation, additive on-wire compatibility, observability, and per-org cost metering.

Non-goals: building the agent's dialog policy/prompt logic (that's the customer's LLM app), a
telephony carrier/number-provisioning product, and TTS model training (we host an open TTS).

## 3. Current state in Orpheus   (file:line, patterns to build on)

- **Streaming engine.** `StreamSession` (`streaming.py:142`) buffers PCM16 mono, decodes on a
  cadence (`add_audio`, `:164`), emits `partial`/`final`/`done`, and already has an **energy VAD**
  (`_trailing_silence`, `streaming.py:234`) plus endpointing that flushes on a detected pause
  (`streaming.py:189`). Turn events are a natural extension of this VAD.
- **Control frames.** The WebSocket parses `start`/`finalize`/`stop`/`close` control JSON
  (`streaming.py:298`) and replies `ready`; `start` can set `sample_rate` (`:300`). New event
  types slot into the same `ws.send_text(json.dumps(ev))` path (`streaming.py:286`).
- **Go relay.** `streaming_ws.go:114` (`StreamTranscribe`) upgrades the browser socket, dials the
  worker WS (`:143`), and pumps both directions (`relay`, `:169`), metering PCM bytes server-side
  (`:184`, `:196`). It is transport-agnostic — new event types flow through unchanged.
- **Auth.** Short-lived HMAC stream token bound to `(session_id, org)` minted at session create
  (`streaming_ws.go:64`, `MintStreamToken`) and verified on the socket (`:73`). Sessions are
  org-scoped/RLS (`streaming.go:75`).
- **Session control plane.** `POST /v1/streaming/sessions` (create), `/finalize`, list/get
  (`server.go:255`), backed by `streaming_sessions` (`streaming.go:77`) with status
  `connecting→live→closing→closed` (`streaming_ws.go:160`,`:202`) and billing on metered seconds
  (`streaming.go:29`).
- **LLM layer.** Provider-agnostic `get_llm()` with a task interface + `complete()`
  (`llm.py:265`, `:133`) and a Modal-hosted vLLM option (`orpheus_llm.py`). This is the LLM half of
  the speech-to-speech loop.
- **Modal GPU pattern** for a TTS service mirrors `orpheus_transcribe.py` (shared-secret endpoint,
  scale-to-zero).

## 4. Proposed design   (architecture, models/algorithms, new processors/endpoints/schema, API shapes, where it runs)

Every subsystem below is specified to a production bar. The cross-cutting requirements — error
handling / graceful degradation, scale & concurrency limits, multi-tenant security & data
handling, additive on-wire compatibility, observability, and cost metering — are called out per
subsystem and consolidated in §4.6.

### 4.1 Turn-taking events (M1) — in the streaming engine

Extend `StreamSession` (`streaming.py:142`) with a `TurnDetector` that runs off the same buffered
audio the VAD already sees. New `StreamConfig` fields (`streaming.py:104`): `backchannel_words`
(e.g. "mm-hm, yeah, right, okay"), `backchannel_max_seconds` (~0.8), `endpoint_silence_seconds`
(reuse `vad_silence_seconds`), `barge_in_enabled`, `addressed_only` + `wake_phrases`.

New events emitted through the existing send path (`streaming.py:286`):
- `speech_start` / `speech_end` — VAD edges (rising/falling energy vs. the RMS ratio at
  `streaming.py:244`), the atoms all other turn logic builds on.
- `barge_in` (#351) — `speech_start` while the server believes TTS/bot audio is playing. The
  streaming session tracks an `agent_speaking` flag set/cleared by the speech-to-speech loop (or by
  a client control frame `agent_state`), and raises `barge_in` so the orchestrator can stop TTS.
- `backchannel` (#352) — a short (< `backchannel_max_seconds`) confirmed utterance whose
  normalized text is in `backchannel_words`; emitted **instead of** `endpoint` so the agent keeps
  its turn. Reuses word normalization `_norm` (`streaming.py:98`) and LocalAgreement finals.
- `endpoint` (#353) — a genuine end-of-turn (silence ≥ `endpoint_silence_seconds` after non-
  backchannel speech). This is the "the user finished, respond now" signal.
- `addressed` (#353) — when `addressed_only=true`, only turns containing a wake phrase (or a
  learned addressee classifier, shipped in M3) set `addressed:true`; others are transcribed but
  flagged `addressed:false` so the agent ignores ambient speech.
- `voicemail` (#354) — a detector over the first few seconds: beep-tone detection (spectral peak)
  + a phrase classifier ("please leave a message after the tone") over early `final` text via
  `get_llm().complete()` (`llm.py:133`). Emits `voicemail:{detected, confidence}` early so an
  outbound agent can drop or leave a message.

All events carry `session_id`-relative timestamps consistent with existing `final.start/end`
(`streaming.py:199`). Backward compatibility: existing clients ignore unknown `type`s; a `start`
frame field `events: ["turn","backchannel","voicemail"]` opts a session into the new stream. Any
event `type` a client did not opt into is suppressed at the send path (`streaming.py:286`), so a
legacy client's byte stream is unchanged.

**Failure modes & graceful degradation.** The `TurnDetector` runs inline with the existing decode
cadence and must never block transcription: if the beep/phrase voicemail classifier or an
`agent_state` frame throws or times out, the detector logs, drops that single event, and the core
`partial`/`final`/`done` stream continues unaffected (transcription is the floor of degradation).
VAD false positives (barge-in on a cough or line noise) are gated by a min-duration threshold and
the existing energy-ratio (`streaming.py:114`,`:244`); a `barge_in` requires sustained speech ≥
`barge_in_min_ms` before firing, and a spurious `speech_start` that resolves under threshold emits
no turn event. If `agent_speaking` is never cleared (loop crash), a watchdog clears it after a
bounded `agent_speaking_max_seconds` so the session cannot get wedged in a permanent barge-in
state. LLM-backed voicemail classification failures fall back to beep-tone-only detection.

**Scale & concurrency.** Turn detection adds only O(1) state per session on top of the existing
buffer, so per-session cost is bounded and the worker's existing per-container session cap governs
concurrency. No extra GPU is used in M1 (energy VAD + heuristics run on CPU); the LLM voicemail
classifier reuses the shared `get_llm()` pool and is rate-limited per session to a single early
call so a burst of new sessions cannot fan out unbounded LLM calls. Backpressure inherits the
existing buffered-decode cadence; if a client outpaces decode, audio is coalesced, not queued
unboundedly.

**Multi-tenant security.** No new trust boundary: events ride the existing `(session_id, org)`
HMAC stream token (`streaming_ws.go:64`,`:73`) and org-scoped/RLS sessions (`streaming.go:75`).
Turn events carry no cross-session data. Raw audio is not persisted by the detector — it works over
the in-memory buffer only.

### 4.2 Telephony ingestion (#355, M4)

A new **media-ingest service** terminates SIP/RTP and WebRTC and re-emits PCM16 mono @ 16 kHz into
the *same* worker streaming WS the browser uses — so the engine above is transport-agnostic:
- **SIP/RTP:** a gateway (drachtio/FreeSWITCH or `aiortc` for WebRTC, `pjsip`/`baresip` for SIP)
  decodes the RTP audio (μ-law/A-law 8 kHz → resample to 16 kHz, reusing PRD-04
  `telephony_denoise`), and forwards frames to the relay exactly like the browser (`streaming_ws.go:184`).
- **DTMF:** RFC 2833 telephone-events (or in-band) decoded to `dtmf:{digit}` control events on the
  same socket, surfaced to the orchestrator for IVR navigation.
- **Call transfer (#355):** a control endpoint `POST /v1/streaming/sessions/{id}/transfer`
  (new, next to `/finalize` at `server.go:258`) triggers a SIP REFER on the gateway.
- Ingest auth reuses `MintStreamToken` (`streaming_ws.go:64`) — the gateway is just another
  authenticated client of the relay, presenting an org-scoped stream token bound to the telephony
  `session_id`.

**Failure modes & graceful degradation.** SIP/RTP is lossy and NAT-hostile, so the ingest service
is built for disconnect first. On RTP packet loss it emits silence/PLC frames rather than stalling
the decoder; on a gateway↔relay WebSocket drop it reconnects with the same stream token and resumes
the session if still `live`, otherwise finalizes it cleanly (`streaming_ws.go:202`). A SIP BYE or
media timeout transitions the session `closing→closed` and flushes a `final`/`done`. A failed
`/transfer` (SIP REFER rejected) returns a structured error and leaves the call up rather than
dropping it. Codec-negotiation failures reject the INVITE with a clear SIP status; they never crash
the gateway process.

**Scale & concurrency.** The media-ingest gateway runs with an explicit **per-session concurrency
cap** and a global `max_calls` ceiling; new INVITEs past the ceiling are rejected with SIP 486/503
(backpressure at the edge) rather than degrading live calls. Each call is one bounded PCM stream
into one worker session, so worker-side scaling reuses the existing per-container session cap.
STUN/TURN and RTP jitter buffers are sized per call and bounded. Gateway containers scale
horizontally behind the relay; CPU-bound resample/denoise is capped per call.

**Multi-tenant security, consent & data handling.** Telephony audio is the most sensitive surface,
so: the gateway authenticates per call with an org-scoped stream token; a call for org A can only
attach to a `session_id` owned by org A (verified at `streaming_ws.go:73`), giving cross-org
isolation identical to the browser path. **Raw call audio is transient** — decoded PCM lives only
in the in-memory session buffer and is **not persisted beyond the session** (no call recording by
default). Any egress of call audio to an external LLM/TTS is gated by the per-org
`allow_external_llm`-style consent flag (§4.6); orgs without it are pinned to Modal-hosted models.
Consent/announcement (e.g. "this call may be processed") is the customer's responsibility but the
per-org gate is enforced server-side. This satisfies GDPR data-minimization: no raw-audio retention,
org-scoped access, and metering-only metadata persisted.

### 4.3 Full-duplex speech-to-speech (#356, M5)

An orchestration loop (a new worker service, `orpheus_workers/voice_agent.py`) that consumes the
turn events above and drives: on `endpoint` → assemble the confirmed transcript → `get_llm()`
(`llm.py:265`) → stream tokens → a new `orpheus-tts` Modal service → stream TTS PCM back through
the relay. On `barge_in` (§4.1) it cancels the in-flight LLM/TTS stream immediately. The relay
already pumps bytes both ways (`streaming_ws.go:184`), so TTS audio rides the return channel.
Integration seam: the loop is optional and pluggable — customers can instead consume raw events
and run their own Pipecat/LiveKit pipeline, using Orpheus purely as ASR + turn detection + TTS.

**Failure modes & graceful degradation.** Each hop has an explicit fallback:
- **LLM timeout / error** in the loop → the loop enforces a per-turn deadline (`llm_turn_timeout`);
  on breach it cancels the LLM stream and emits a configurable fallback utterance ("sorry, could
  you repeat that?") through TTS, then returns the turn to the user rather than hanging silent.
- **Modal TTS cold-start or failure** → the worker TTS client (`tts.py`) mirrors `transcribe.py:39`
  and carries a `local` Piper fallback; if the `orpheus-tts` Modal endpoint is cold past a deadline
  or returns an error, the loop degrades to the local fallback voice so audio still flows. First
  request after scale-to-zero pays cold-start; the loop streams a short filler/earcon while waiting
  so first-byte latency is masked.
- **Barge-in race** → `barge_in` cancels the in-flight LLM and TTS streams via a cancellation token;
  TTS chunks already queued in the relay are drained/stopped and `agent_speaking` is cleared. If
  cancellation itself fails, the `agent_speaking` watchdog (§4.1) still recovers the session.
- **Relay/gateway disconnect mid-response** → the loop stops generation (no wasted GPU/tokens) and
  the session finalizes cleanly.

**Scale & concurrency.** The loop holds one bounded in-flight LLM+TTS pipeline per session; a
per-org and per-session concurrency cap bounds simultaneous speech-to-speech turns so one tenant
cannot exhaust the LLM pool or TTS GPUs. LLM calls reuse the pooled `get_llm()`; TTS calls are
subject to the `orpheus-tts` Modal `max_containers` ceiling (§4.4), and requests past it queue with
a bounded wait then fall back to local TTS (backpressure, not unbounded queueing). Token generation
is capped per turn (`max_response_tokens`) to bound cost and latency.

**Multi-tenant security & data handling.** The loop runs entirely within the org-scoped session; the
assembled transcript and any audio sent to LLM/TTS are gated by the per-org `allow_external_llm`-style
flag (§4.6). No transcript or audio is persisted beyond the session except metering counters. TTS
audio is generated per session and never shared across orgs.

### 4.4 New Modal service: `orpheus-tts`

`infra/modal/orpheus_tts.py` mirroring `orpheus_transcribe.py`: `@app.cls(gpu="a10g",
min_containers=0, max_containers=<capped>, scaledown_window=300, secrets=[auth])`, `@modal.enter`
warming an open TTS model (e.g. Kokoro / XTTS / Piper — non-gated, per the ECAPA/Qwen precedent),
shared-secret `@modal.fastapi_endpoint` streaming PCM chunks. Worker client `tts.py` mirrors
`transcribe.py:39` (`ORPHEUS_MODAL_TTS_URL/_TOKEN`, `local` Piper fallback for tests).

**Scale, cost, and cold-start.** `min_containers=0` keeps idle cost at zero and `scaledown_window`
=300 avoids thrash; `max_containers` is set to a hard ceiling so a traffic spike cannot fan out GPU
cost unbounded. For latency-sensitive deployments an optional `min_containers=1` warm pool trades a
fixed GPU-second cost for sub-cold-start first-byte latency (documented as a per-deployment knob).
A per-session concurrency cap on the TTS endpoint bounds GPU contention. **Failure isolation:** the
shared-secret endpoint rejects unauthenticated callers; on any TTS error or cold-start-timeout the
worker client returns a typed error that the loop (§4.3) converts to the local Piper fallback.

**Security.** The endpoint is shared-secret (`orpheus-modal-auth` Secret) like
`orpheus_transcribe.py`; only the worker, holding `ORPHEUS_MODAL_TTS_TOKEN`, can call it. Audio in
is transient — synthesized and streamed back, not stored.

### 4.5 Schema

Add nullable columns / a child table to `streaming_sessions` (`streaming.go:77`) for
`turn_events_count`, `barge_in_count`, `voicemail_detected` — org-scoped/RLS like all tables
(README convention). Add metering counters (§4.6) `tts_gpu_seconds`, `llm_tokens`,
`metered_pcm_seconds` on the same row/child table so cost is attributable per org without
persisting any raw audio or transcript. No new core tables required for the turn-taking layer.

### 4.6 Cross-cutting production requirements

**Backward-compatible additive on-wire shape.** All new WS event `type`s (`speech_start`,
`speech_end`, `barge_in`, `backchannel`, `endpoint`, `addressed`, `voicemail`, `dtmf`) and new
control frames (`agent_state`, `transfer`) are **additive**. Existing clients ignore unknown
`type`s (§4.1); new behavior is **opt-in** via the `start.events` field, so a session that does not
opt in receives byte-for-byte the same stream it does today. New session-control endpoints
(`/transfer`) sit next to existing ones (`server.go:258`) and are additive to the REST surface.

**Observability.** Emit metrics and structured logs (org-tagged, session-tagged, never
raw-audio-bearing): per-session and per-org counts of each turn event (`speech_start`,
`barge_in`, `backchannel`, `endpoint`, `voicemail`), **barge-in cancel latency** (event → TTS stop),
**voicemail-detection rate** and confidence distribution, **TTS first-byte latency** (and cold-start
occurrences), speech-to-speech **endpoint→first-TTS-byte latency**, LLM turn latency/timeouts,
telephony call setup/teardown and packet-loss/PLC counts, and **per-session error counts** by
category (LLM timeout, TTS failure/cold-start, gateway disconnect, cancellation failure). These
back the numeric acceptance targets in §6 and feed dashboards/alerts.

**Cost metering (per-org attribution).** Extend the existing metered-seconds billing
(`streaming.go:29`, PCM metered at `streaming_ws.go:196`) with: **metered PCM seconds** (ASR, as
today, now also for telephony ingest), **TTS GPU-seconds** attributed from the `orpheus-tts` Modal
service per session/org, and **LLM tokens** consumed by the speech-to-speech loop per session/org.
All three are written to the metering counters in §4.5 keyed by org, so telephony, TTS, and
speech-to-speech usage bill cleanly per tenant.

**Per-org external-egress gate.** A per-org `allow_external_llm`-style flag governs whether audio,
transcripts, or tokens may leave to any non-Modal-hosted LLM/TTS. Enforced server-side in the loop
(§4.3) and ingest (§4.2); orgs without it are pinned to Modal-hosted models and no raw call audio
egresses. This is the consent/GDPR control point.

## 5. Rollout / milestones

Milestones are **ordering only** — each is production-grade and independently shippable with the
full §4.6 requirements (error handling, scale caps, tenant isolation, observability, metering). No
milestone is a reduced-scope prototype; the complete feature set ships across M1–M5.

1. **M1 — Turn-taking events (production).** `speech_start`/`speech_end`/`endpoint` from the VAD
   (`streaming.py:234`), plus `backchannel` (#352), wake-phrase `addressed` gating (#353),
   `voicemail` beep+phrase detector (#354), and `agent_speaking`/`barge_in` (#351). `start.events`
   opt-in; relay untouched. Ships with min-duration false-positive gating, the `agent_speaking`
   watchdog, per-event observability, and metering counters. Shippable production turn-detection
   layer on its own.
2. **M2 — `orpheus-tts` Modal service + client (production).** `infra/modal/orpheus_tts.py` with
   capped `max_containers`, warm-pool knob, shared-secret auth, and `tts.py` with local Piper
   fallback and TTS-GPU-second metering. Usable standalone as a hosted TTS endpoint.
3. **M3 — Learned addressee/backchannel classifier.** Replaces/augments the M1 heuristics with a
   learned addressee classifier for `addressed` and a learned backchannel model, behind the same
   event contract (no on-wire change). Production quality with fallback to M1 heuristics on model
   error.
4. **M4 — Telephony ingestion (production).** SIP/RTP + WebRTC ingest service, DTMF, and
   `/transfer` (#355), with reconnect/PLC handling, `max_calls` ceiling, transient-audio /
   consent-gate handling, and per-call metering. One gateway implementation, production-hardened.
5. **M5 — Full-duplex speech-to-speech (production).** `voice_agent.py` loop (#356) composing
   ASR → LLM → TTS with barge-in cancellation, per-turn LLM/TTS deadlines and fallbacks, per-org
   concurrency caps, and LLM-token + TTS-GPU-second metering. The complete conversational path.

## 6. Verification / acceptance criteria   (concrete e2e tests)

The bar is **production**: unit tests below are retained, and end-to-end tests run against a **real
worker + real (or warm) Modal TTS/LLM service and the real streaming relay**, exercising success,
failure, latency-target, and multi-tenant-isolation paths.

**Engine unit tests (no network).** `StreamSession` is transport-free and takes an injectable
transcriber (`streaming.py:151`), so drive it with synthetic PCM + a fake transcriber:
- Feed speech then `backchannel_max_seconds` of "yeah" → assert exactly one `backchannel`, no
  `endpoint`.
- Feed speech then `endpoint_silence_seconds` of silence → assert one `endpoint`.
- With `agent_speaking=true`, inject speech → assert `barge_in`; inject a sub-`barge_in_min_ms`
  blip → assert **no** `barge_in` (false-positive gate).
- Feed a beep + "leave a message" text → assert `voicemail.detected=true`; with LLM classifier
  stubbed to throw → assert graceful fallback to beep-only detection, no crash.
- Never clear `agent_speaking` → assert the watchdog clears it after `agent_speaking_max_seconds`.

**Relay e2e (real relay).** Existing streaming relay/integration tests (`streaming_ws_test.go`,
`streaming_integration_test.go`) still pass; add tests asserting new event types pass through the
relay unmodified, are billed on metered PCM (`streaming_ws.go:196`), and that a client which does
**not** set `start.events` receives an unchanged (legacy) stream (additive-compatibility check).

**Addressed gating.** With `addressed_only=true`, ambient speech without a wake phrase yields
`addressed:false`; a wake-phrase turn yields `addressed:true`.

**Speech-to-speech e2e (real worker + warm Modal LLM+TTS).**
- **Latency target, measured e2e:** user `endpoint` → **first TTS byte < 1.5 s** with Modal LLM+TTS
  warm; report the p50/p95 from the observability metrics (§4.6).
- **Barge-in target, measured e2e:** a `barge_in` mid-utterance cancels in-flight TTS within
  **< 200 ms** (event → TTS stop), including a **barge-in-under-load** variant with N concurrent
  sessions to prove the cancel latency holds under the concurrency cap.
- **Modal TTS down → graceful fallback:** with the `orpheus-tts` endpoint forced to error/cold-timeout,
  assert the loop degrades to the local Piper voice and audio still flows (no dead air, no crash).
- **LLM timeout → fallback utterance:** force an LLM stall past `llm_turn_timeout`; assert the loop
  cancels and emits the fallback utterance, returning the turn to the user.
- **Malformed audio:** feed corrupt/garbage PCM frames; assert the session stays up, logs an error,
  and does not emit spurious turn events or crash the worker.

**Telephony e2e (M4).** A recorded μ-law RTP sample played into the real gateway produces the same
transcript as the equivalent 16 kHz WAV through the browser path (± WER tolerance); a DTMF tone
produces the matching `dtmf` event; a mid-call gateway↔relay disconnect reconnects and resumes (or
finalizes cleanly); a rejected SIP REFER on `/transfer` returns a structured error and leaves the
call up; INVITEs past `max_calls` are rejected (486/503) without degrading live calls.

**Multi-tenant isolation (required).** With two orgs A and B: assert org A's stream token cannot
open, read, or attach to org B's streaming session **or** telephony session (verified against
`streaming_ws.go:73` and org-scoped RLS `streaming.go:75`) — the socket/REST call is rejected. Assert
metering counters (PCM seconds, TTS GPU-seconds, LLM tokens) are attributed to the correct org and
never cross tenants, and that no raw call audio is persisted after the session closes.

## 7. Dependencies, risks, open questions

- **Dependencies:** PRD-04 `telephony_denoise` (8 kHz), `orpheus-modal-auth` Secret, Modal GPU for
  TTS, a SIP/WebRTC gateway (aiortc/FreeSWITCH), `orpheus-llm` (`orpheus_llm.py`).
- **Risks:** turn-detection false positives (barge-in on background noise) — mitigate with the
  existing energy-ratio VAD tuning (`streaming.py:114`) plus a min-duration gate (§4.1). Telephony
  is operationally heavy (NAT/STUN/TURN, codec zoo); M4 ships one production-hardened gateway with a
  `max_calls` ceiling. TTS cold-start latency — mitigate with the warm-pool knob (§4.4) and local
  fallback (§4.3).
- **Latency:** speech-to-speech is latency-critical; keep ASR on the low-latency streaming path
  and TTS chunk-streamed, not batch; enforce per-turn LLM/TTS deadlines (§4.3).
- **Open questions:** learned addressee/backchannel classifier (M3) tuning vs. the M1 heuristics;
  whether to expose turn events as SSE for non-WebSocket consumers (additive if added); the exact
  per-org data-egress policy default for audio to Modal TTS (reuse the `allow_external_llm`-style
  gate, §4.6); barge-in signalling when the client owns TTS (client sends `agent_state`) vs. the
  server-driven loop.

## 8. Effort

Effort is grouped by milestone (§5); each milestone is production-grade, including its §4.6
cross-cutting work (error handling, scale caps, isolation, observability, metering), not a prototype.

- **M1** — Turn events (VAD edges, backchannel, addressed, voicemail, barge-in) in the engine +
  relay passthrough + false-positive gating + watchdog + observability + tests: **~3 wk**.
- **M2** — `orpheus-tts` Modal service + client (caps, warm-pool knob, local fallback, metering):
  **~1.5 wk**.
- **M3** — Learned addressee/backchannel classifier behind the M1 event contract: **~2 wk**.
- **M4** — Telephony ingest service (SIP/RTP+WebRTC, DTMF, transfer, reconnect/PLC, caps, consent
  gate): **~4–5 wk** (the heaviest slice).
- **M5** — Speech-to-speech orchestration loop + barge-in cancellation + deadlines/fallbacks +
  metering: **~2 wk**.
- Total: **~3 wk** for the M1 turn-taking layer alone (shippable); **~12–14 wk** for the full,
  production-grade agent substrate across M1–M5.
