# PRD: Realtime Streaming Enhancements

**Status:** Proposed · **Priority:** P1 · **Epic:** Realtime ASR · **Related issues:** #306 (sub-300 ms partials), #308 (semantic turn detection), #307 (in-stream diarization), #309 (eager/speculative end-of-turn), #310 (interim per-word confidence), #312 (realtime PII redaction)

## 1. Summary

Orpheus already ships realtime transcription: a LocalAgreement-2 engine
(`apps/workers/src/orpheus_workers/streaming.py`) behind an authenticated Go
relay (`apps/api/internal/handlers/streaming_ws.go`) that emits `ready` /
`partial` / `final` / `done` events over a WebSocket. This PRD turns that
functional-but-basic pipeline into a competitive realtime product by adding six
capabilities that voice-agent and live-captioning customers expect, all built on
the existing transport-free `StreamSession` state machine and the existing
event protocol:

1. **Sub-300 ms partials** (#306) — faster, more frequent provisional output.
2. **Semantic turn detection** (#308) — end-of-turn from a Smart-Turn model, not
   just energy silence.
3. **In-stream diarization** (#307) — per-word speaker labels using ECAPA.
4. **Eager / speculative end-of-turn with resume** (#309) — emit an early
   end-of-turn and cleanly retract/resume if the speaker continues.
5. **Interim per-word confidence** (#310) — confidence on partials and finals.
6. **Realtime PII redaction** (#312) — mask PII in the outbound text stream.

## 2. Motivation & goals

**Problem.** The current engine re-decodes only every
`min_chunk_seconds = 1.0` (`streaming.py:107`), so first-partial latency is ~1 s+
— too slow for interactive voice agents. Endpointing is pure energy VAD
(`_trailing_silence`, `streaming.py:234-244`), which fires on breath pauses and
misses semantic turn ends, producing premature or late `final`s. There is no
speaker attribution in-stream (diarization is batch-only, `diarize.py`), no
confidence on streamed words (`_extract_words` drops the `confidence` field,
`streaming.py:127-139`), and no redaction on the live text path (redaction is a
batch processor, `redact.py`/`processors/redact.py`). These gaps block
voice-agent, live-caption, and compliance use cases.

**Measurable goals.**
- p50 first-partial latency < 300 ms, p95 < 500 ms (measured relay-in →
  first `partial` out).
- Semantic end-of-turn: median endpoint error < 300 ms vs. human-labeled turn
  ends on a dialog set; ≥ 30% fewer false endpoints than energy-VAD baseline.
- Per-word confidence present on ≥ 99% of `final` words.
- 0 unredacted PII tokens reach the client when `redact.enabled` on a labeled
  live stream.
- In-stream speaker label agreement ≥ 85% vs. batch diarization on the same audio.

**Non-goals.** Changing the HMAC token/relay auth model
(`streaming_ws.go:64-101`); server-side billing metering (already server-metered,
`streaming_ws.go:196`); batch diarization quality (prd-03); multi-party speaker
*identification* by name (anonymous S1..Sn only, consistent with `diarize.py:9`).

## 3. Current state in Orpheus

- **Engine** `streaming.py`: `StreamSession` (line 142) LocalAgreement-2 —
  `add_audio` (line 164) buffers until `min_chunk_samples` then `_decode`
  (line 177); confirms the longest common prefix of two consecutive hypotheses
  (`_common_prefix_len`, line 247); emits `final` (line 197) and `partial`
  (line 224); trims the buffer at confirmations to bound cost (line 211-220).
  `StreamConfig` (line 103) holds `min_chunk_seconds`, `max_buffer_seconds`,
  `vad_silence_seconds`, `vad_energy_ratio`, `vad_enabled`.
- **Transcriber injection** (line 48, `Transcriber` type): default
  `whisper_transcriber` (line 79) dumps PCM→wav→Whisper with `word_timestamps=True`;
  tests inject a fake. Word confidence *exists* upstream
  (`transcribe.py:210`) but is **dropped** by `_extract_words` (line 127-139).
- **WebSocket app** `create_app` (line 256): control frames `start` /
  `finalize|stop|close` (line 299-308), binary PCM frames → `add_audio` (line 285).
- **Relay** `streaming_ws.go`: mints/validates HMAC stream token (line 64-101),
  dials worker WS (line 143), bidirectional pump (line 169-186), server-side PCM
  metering (line 176-197). It relays frames verbatim — protocol additions are
  transparent to it.
- **ECAPA embeddings** `infra/modal/orpheus_diarize.py` (`_embed`, line 72) and
  worker `ModalDiarizer` (`diarize.py:84`) — reusable for streaming speaker
  vectors.

## 4. Proposed design

All additions are **backward compatible**: new event types and new fields on
existing events; clients that ignore unknown fields keep working. The Go relay
needs **no change** — it pumps opaque frames.

### 4.1 Sub-300 ms partials (#306)

- Lower the decode cadence: add `StreamConfig.partial_chunk_seconds = 0.25` and
  decode a *partial-only* pass at that cadence, while keeping the ~1 s cadence for
  the more expensive agreement/commit pass. `add_audio` (line 164) tracks two
  counters (`_samples_since_partial`, `_samples_since_commit`).
- Use a small/fast model for the partial pass. Reuse the per-request model cache
  in `transcribe.py:27`; set the streaming partial model via
  `ORPHEUS_STREAMING_PARTIAL_MODEL` (default `tiny.en`), commit pass via the
  existing `ORPHEUS_WORKER_WHISPER_MODEL`.
- Optionally route the commit/agreement pass to the Modal GPU transcribe endpoint
  (`_transcribe_modal`, `transcribe.py:39`) for accuracy while partials stay
  local for latency.

### 4.2 Semantic turn detection (#308)

- Introduce a pluggable `EndpointDetector` protocol (mirrors `redact.py`'s
  `get_detector()` selection). `EnergyEndpointDetector` wraps today's
  `_trailing_silence` logic; `SmartTurnDetector` runs a lightweight end-of-turn
  classifier (e.g. a Smart-Turn / turn-taking model) over the recent audio +
  provisional text and returns `(is_endpoint, prob)`.
- **Where it runs.** Small model → run in the worker CPU process; if it needs a
  GPU, deploy a `orpheus_turn.py` Modal service on the `orpheus_diarize.py`
  pattern (`@app.cls` + `@modal.enter` + fastapi endpoint + shared secret), called
  every commit pass. Selected by `ORPHEUS_STREAMING_TURN_BACKEND`
  (`energy` | `local` | `modal`), default `energy`.
- Replace `endpoint = flush or (vad_enabled and self._trailing_silence())`
  (`streaming.py:189`) with `endpoint = flush or self._endpoint_detector.check(...)`.
  Energy VAD becomes one strategy behind the same seam.

### 4.3 In-stream diarization (#307)

- On each committed word window, compute an ECAPA embedding for that audio span
  (reuse `orpheus_diarize.py:_embed`, line 72) and assign it to the nearest
  online speaker centroid (cosine), spawning a new centroid `S{n+1}` when distance
  exceeds a threshold (online agglomerative — no need to know speaker count up
  front). Centroids are session state on `StreamSession`.
- **Where it runs.** Embeddings are GPU-cheap but need the ECAPA model → call the
  diarize Modal endpoint (`ORPHEUS_MODAL_DIARIZE_URL`) with the committed span, or
  add a `/embed` method to `orpheus_diarize.py` returning raw vectors so the worker
  does the online clustering. Gated by `ORPHEUS_STREAMING_DIARIZE=1`.
- Attach `speaker` to `final` events (and `partial` best-effort). Speaker labels
  can retro-correct: emit a `speaker_update {word_range, speaker}` event when a
  centroid merge relabels earlier words.

### 4.4 Eager / speculative end-of-turn with resume (#309)

- When `SmartTurnDetector` returns `prob` in a "likely but not certain" band, emit
  a new `endpoint_speculative {turn_id, text}` event and start finalizing
  optimistically, but keep the audio buffer for a short grace window
  (`StreamConfig.eager_resume_seconds = 0.4`).
- If speech resumes within the window, emit `endpoint_cancel {turn_id}` and
  continue the same turn (do **not** re-emit already-confirmed finals). If the
  window elapses, promote to a normal `final` + turn close.
- Turn identity: add `turn_id` (monotonic int) to `partial`/`final`/`done` so
  clients can group words and handle retraction. Reuse the existing trim logic
  (`streaming.py:211-220`) for buffer management across the grace window.

### 4.5 Interim per-word confidence (#310)

- Stop dropping confidence: extend `_extract_words` (`streaming.py:127-139`) to
  carry `confidence` (already produced upstream, `transcribe.py:210`).
- Add `words: [{word, start, end, confidence}]` to `partial` and `final` events
  (in addition to the existing flat `text`). Clients can render low-confidence
  words differently or gate voice-agent actions on a confidence floor.

### 4.6 Realtime PII redaction (#312)

- Reuse `redact.py` (`get_detector()`, `redact_text`, line 104/125) on the
  outbound stream. Add a `StreamSession` redaction hook applied to `final`/`partial`
  `text` and per-word tokens **before** they are emitted.
- Streaming caveat: PII can span the partial→final boundary. Only redact on
  **committed (final)** tokens for guaranteed correctness; on partials, apply
  best-effort regex redaction and mark `redacted_provisional: true` so clients
  know a partial may still leak until confirmed. `RegexDetector` (`redact.py:56`)
  is the low-latency default; Presidio is too slow for the hot path (document as
  final-only).
- Enabled via the `start` control frame: `{type:"start", redact:{enabled:true,
  entities:[...], mask:"type"}}` — parsed in `create_app` alongside `sample_rate`
  (`streaming.py:299-302`). Emit a `redactions` summary in the `done` event
  mirroring `maybe_redact`'s return (`redact.py:179`).

### 4.7 Protocol additions (summary)

| Event | New / changed |
| --- | --- |
| `start` (in) | + `redact`, + `diarize`, + `turn_detector`, + `partial_ms` |
| `partial` | + `words[]` (w/ `confidence`), + `turn_id`, + `speaker?`, + `redacted_provisional?` |
| `final` | + `words[]` (w/ `confidence`), + `turn_id`, + `speaker?` |
| `endpoint_speculative` (new) | `{turn_id, text}` |
| `endpoint_cancel` (new) | `{turn_id}` |
| `speaker_update` (new) | `{turn_id, word_range, speaker}` |
| `done` | + `redactions[]`, + `speakers[]` |

### 4.8 Production hardening (all six capabilities)

Realtime is unforgiving; every capability ships with these built in, not bolted on
later.

- **Error handling & failure modes / graceful degradation.** Each enrichment path
  is independently fail-open on the *stream*, never fail-closed: if the Modal
  turn/diarize/embedding call times out (`ORPHEUS_STREAMING_MODAL_TIMEOUT_S`,
  default 2 s) or errors, the session **degrades to energy-VAD endpointing and drops
  the speaker/turn enrichment for that window**, emitting the plain
  `partial`/`final` it always could, plus a one-shot `warning` control event. The
  one exception is redaction: PII redaction is fail-**closed** — if the detector
  throws on a committed token, that token is masked wholesale (masked-on-error) so a
  detector crash can never leak PII. Dual-cadence partials degrade to single-cadence
  if the fast partial model fails to load. Bounded retries (max 1 on the hot path to
  protect latency) with immediate fallback; no retry on 401. A worker that loses the
  Modal endpoint entirely still serves a fully functional LocalAgreement-2 stream.
- **Scale, concurrency & backpressure.** A per-worker concurrent-stream cap
  (`ORPHEUS_STREAMING_MAX_SESSIONS`) is enforced at `create_app` accept time; over
  the cap, `start` is rejected with a `busy` close code so the relay can shed load
  rather than thrash. Dual-cadence decoding raises CPU per stream, so the partial
  pass uses the small local model and the commit pass can offload to the Modal GPU
  transcribe endpoint (`_transcribe_modal`) under a per-stream in-flight cap of 1
  (coalesce, don't queue) to bound cost. If a client sends PCM faster than realtime,
  `add_audio` applies backpressure via the existing `max_buffer_seconds` trim
  (line 211-220) and drops the oldest un-decoded audio rather than growing
  unbounded. Modal turn/diarize calls are rate-limited per session so a chatty
  stream can't fan out GPU calls without bound.
- **GPU/CPU limits & bounded cost.** Partial pass is CPU-pinned (`tiny.en`); GPU is
  reserved for commit/turn/embedding and gated by env flags so a CPU-only deploy
  runs the whole product minus the GPU enrichments. Per-session GPU-seconds are
  accumulated and exposed so a runaway stream can be capped
  (`ORPHEUS_STREAMING_MAX_GPU_SECONDS_PER_SESSION`) → beyond it, enrichment
  auto-disables and the stream continues plain.
- **Multi-tenant security & RLS.** The relay's HMAC stream token
  (`streaming_ws.go:64-101`) is unchanged and remains the tenant boundary; it
  already binds `org_id`. Audio spans sent to Modal turn/diarize services are
  transient (never persisted). Streaming session rows and any per-turn records stay
  RLS-scoped to the token's `org_id`. Redaction config from the `start` frame cannot
  widen scope. New Modal services enforce the shared-secret token exactly as
  `orpheus_diarize.py:173-183`.
- **On-wire backward compatibility.** Every addition is a new event type or a new
  field on an existing event (§4.7); the Go relay pumps opaque frames and needs **no
  change**. A client that ignores unknown fields/events still gets correct
  `ready`/`partial`/`final`/`done`. New `start` fields all default off. The
  LocalAgreement invariant (finals never re-sent, `streaming.py:9`) is preserved —
  `turn_id` groups words but never rewrites a confirmed final; retraction happens
  only via explicit `endpoint_cancel` before promotion.
- **Observability.** Per-session structured metrics: `stream_first_partial_ms`
  (histogram), `stream_partial_ms`, `stream_commit_ms`, `stream_endpoint_error_ms`,
  `stream_false_endpoints_total`, `stream_turn_backend_fallback_total`,
  `stream_diarize_fallback_total`, `stream_redactions_total{entity}`,
  `stream_masked_on_error_total`, `stream_gpu_seconds`, `stream_sessions_active`,
  `stream_sessions_rejected_busy_total`. Every degradation logs once at WARN with
  the session id and reason.
- **Cost metering.** PCM byte metering stays server-side in the relay
  (`streaming_ws.go:196`) — unchanged. GPU enrichment adds `stream_gpu_seconds`
  metered through the same `ORPHEUS_WORKER_GPU_COST_USD_PER_SECOND` path and summed
  into the session's `done` event so billing sees the true cost.
- **Config / env surface.** `ORPHEUS_STREAMING_PARTIAL_MODEL`,
  `ORPHEUS_STREAMING_TURN_BACKEND` (`energy`|`local`|`modal`),
  `ORPHEUS_STREAMING_DIARIZE`, `ORPHEUS_STREAMING_MAX_SESSIONS`,
  `ORPHEUS_STREAMING_MODAL_TIMEOUT_S`,
  `ORPHEUS_STREAMING_MAX_GPU_SECONDS_PER_SESSION`, plus Modal URL/token trios for
  the optional `orpheus_turn.py` service. Read once at worker start, logged in the
  startup config; a key-less deploy runs energy-VAD + local partials + regex
  redaction fully.

## 5. Delivery milestones

Milestones are **ordering only**. Each is production-quality, hardened per §4.8,
and independently shippable — none ships a partial or "good enough" version. The
full six-capability scope is committed; milestones sequence delivery so each lands
with its failure paths, metrics, cost metering, and §6 acceptance bar met.

- **M1 — Per-word confidence (#310) + realtime PII redaction (#312),
  production-complete.** Confidence carried on every `partial`/`final` word;
  redaction fail-closed (mask-on-error), final-guaranteed with best-effort
  provisional marking, `done` redaction summary. Pure `streaming.py`, no new infra,
  but shipped with full metrics and the 0-leak acceptance bar green.
- **M2 — Sub-300 ms dual-cadence partials (#306), production-complete.** Fast local
  partial model + commit pass, backpressure + trim, single-cadence degradation on
  partial-model failure, p50 < 300 ms / p95 < 500 ms bar green under concurrent load.
- **M3 — Semantic turn detection (#308), production-complete.** Pluggable
  `EndpointDetector` with `energy` default and hardened `local`/`modal` Smart-Turn
  (timeout → energy fallback), endpoint-error and false-endpoint metrics, ≥ 30%
  fewer false endpoints bar green.
- **M4 — Eager / speculative end-of-turn + resume (#309), production-complete.**
  `endpoint_speculative`/`endpoint_cancel` with `turn_id`, grace-window buffer
  management, no duplicate/lost finals, conservative probability band tuned against
  the dialog set.
- **M5 — In-stream diarization (#307), production-complete.** Online ECAPA
  clustering via the diarize Modal `/embed` path, `speaker` on finals +
  `speaker_update` retro-correction, ≥ 85% batch-agreement bar green, diarize
  fallback on Modal loss.

## 6. Verification / acceptance criteria

Senior QA drives a **real browser client through the live Go relay to a real worker
+ live Modal services** (not fakes). Every numeric target is a hard gate; every
capability is tested on both its happy path and its failure path.

1. **Latency under load:** stream a 16 kHz mic feed while N concurrent sessions run
   near `ORPHEUS_STREAMING_MAX_SESSIONS`; measure relay-in → first `partial`; assert
   p50 < 300 ms, p95 < 500 ms sustained (not just single-stream). Confirm partials
   stabilize into finals (LocalAgreement invariant, `streaming.py:9`).
2. **Backpressure:** push PCM faster than realtime; assert the buffer stays bounded
   by `max_buffer_seconds` (oldest audio trimmed, not OOM), finals stay correct, and
   no unbounded latency growth.
3. **Confidence:** assert every `final` word carries a `confidence` in [0,1]
   (≥ 99% coverage target); a deliberately garbled word shows low confidence.
4. **Redaction (fail-closed):** speak an email + credit-card number; assert the
   `final` text is masked and the `done` event lists entity counts; assert **0
   unmasked PII tokens** in any `final` frame. Then force the detector to throw on a
   token and assert that token is masked-on-error (never leaked), with
   `stream_masked_on_error_total` incremented.
5. **Semantic turn + failure fallback:** on a dialog with mid-sentence pauses,
   assert ≥ 30% fewer false endpoints than energy-VAD baseline and endpoint within
   300 ms of the true turn end. Then kill the turn Modal service mid-stream and
   assert clean degradation to energy VAD (a `warning` event, stream keeps running,
   fallback metric increments).
6. **Eager/resume:** trigger a speculative endpoint then keep talking within the
   grace window; assert an `endpoint_cancel` with the same `turn_id` and no
   duplicate/lost finals; let it elapse and assert promotion to `final`.
7. **Diarization + failure fallback:** two speakers alternate; assert `speaker`
   labels on finals agree ≥ 85% with batch `audio.diarize` on the recorded audio;
   verify `speaker_update` retro-corrects on a late merge. Kill the diarize Modal
   endpoint and assert the stream continues without speaker labels (no crash,
   fallback metric increments).
8. **Cost metering:** assert the `done` event's `stream_gpu_seconds` matches the
   summed Modal enrichment time and that PCM-byte metering in the relay is
   unchanged; exceed `ORPHEUS_STREAMING_MAX_GPU_SECONDS_PER_SESSION` and assert
   enrichment auto-disables while the stream continues plain.
9. **Load shedding:** open sessions past `ORPHEUS_STREAMING_MAX_SESSIONS`; assert
   excess `start`s are rejected with the `busy` close code and
   `stream_sessions_rejected_busy_total` increments — the worker does not thrash.
10. **Multi-tenant isolation:** two orgs stream concurrently with distinct HMAC
    tokens; assert no cross-tenant word/speaker bleed and each session's records are
    RLS-scoped to its `org_id`.
11. **Back-compat:** an old client ignoring new fields/events still renders correct
    `partial`/`final`/`done`; assert the Go relay binary is byte-identical
    (unmodified) across the whole rollout.

## 7. Dependencies, risks, open questions

- **Dependencies:** ECAPA Modal endpoint (already deployed for #307); optional
  Smart-Turn model + possible `orpheus_turn.py` Modal service; `redact.py`
  detectors; a fast partial Whisper model.
- **Risks:** (a) dual-cadence decoding increases worker CPU — cap concurrent
  streams / offload commit pass to Modal GPU. (b) Provisional-partial PII leak is
  inherent — mitigated by `redacted_provisional` + final-only guarantee; document
  clearly. (c) Online diarization drift on short spans — require a min embedding
  window and allow retro-correction. (d) Eager endpoints risk cutting a speaker
  off — tune the probability band conservatively.
- **Open questions:** does the Go relay need a max-frame-size bump for `words[]`
  payloads (`ReadBufferSize` 4096, `streaming_ws.go:104`)? Should turn boundaries
  create separate DB `streaming_sessions` rows or stay one session? Confidence
  calibration source (Whisper prob vs. an external calibrator)?

## 8. Effort

**T-shirt: L** (≈ 6–8 engineer-weeks; each milestone includes failure-path
hardening, metrics, and its acceptance bar).

- M1 (1 wk): confidence + fail-closed realtime redaction + metrics.
- M2 (1 wk): dual-cadence sub-300 ms partials + backpressure + degradation.
- M3 (1.5 wk): pluggable endpoint detector + Smart-Turn + timeout fallback.
- M4 (1.5 wk): eager end-of-turn + resume + `turn_id` protocol.
- M5 (1.5 wk): in-stream ECAPA diarization + `speaker_update` + diarize fallback.
