# PRD — Low-Latency Streaming Synthesis Service (always-on TTS, chunked output, real cancellation)

**Status:** Proposed · **Priority:** P1 (blocks a usable voice interface for the harness) · **Owner:** Sanskar / Alfred
**Scope note:** this is **personal-path infrastructure for the Claude Code harness**, not an Orpheus product
feature, and is not subject to this directory's multi-tenant/RLS conventions. Per the fork-vs-core analysis on
this thread (score 41/45 for "separate integration layer" vs. 17/45 for building it into Orpheus core), the
**implementation** deliberately does not live inside the `orpheus` repo/branch — it gets its own repo
(recommended: `~/alfred/repos/alfred-voice-bridge`, alongside the Harness Voice Adapter this service is built
for). Only this spec document is indexed in `docs/prd/`, at Sanskar's explicit direction, so it numbers
alongside the rest of the project's PRDs (see companion [`prd-27-harness-voice-adapter.md`](prd-27-harness-voice-adapter.md)).
It reads and cites Orpheus source only to reuse proven patterns and to stay compatible with the existing
`/v1/stream/transcribe` seam and Modal TTS fallback — it does not modify Orpheus.

---

## 1. Problem

Orpheus's only synthesis path today is `infra/modal/orpheus_tts.py`, a Modal-hosted Kokoro-82M service with
`min_containers=0` and `scaledown_window=300`. Prior research on this thread (`modalSuitability`) established
two facts about that deployment, firsthand from the code, that are decisive for a personal always-on assistant:

1. **Cold starts are routine, not edge-case, for this usage pattern.** A 5-minute scale-to-zero window
   (`scaledown_window=300`) guarantees a cold container on essentially every conversation that starts after a
   short gap — which is the normal shape of "ask Alfred something a few times a day." Bare container boot is
   ~1s, but Modal's own guidance is explicit that model load (not container boot) dominates cold-start cost for
   anything beyond a trivial payload, and Orpheus's `modal_client.py` is written assuming calls may take a long
   time to return (it follows a 303 long-poll redirect chain with `max_redirects=1000`, `timeout=600.0s`). As
   deployed, with no GPU-memory-snapshotting configured, a realistic cold-start range is **10–60s+** — which by
   itself already blows PRD-05's 1.5s endpoint→first-TTS-byte target on any turn after a 5-minute idle gap,
   before a single token of LLM output is even generated.
2. **The obvious fix is not a config change, it's an irrational cost.** Setting `min_containers=1` on the
   Modal TTS app removes the cold start by keeping an A10G ($1.10/hr, per Modal's own pricing) permanently
   billed: **3 × $1.10/hr × 24 × 30 ≈ $2,376/month** if mirrored across all three voice-path Modal apps, or
   **~$792/month** for TTS alone kept warm 24/7 — to serve one person's bursty, intermittent utterances. That
   is not a tunable; it is a different, worse architecture wearing a config flag.
3. **Even fully warm, synthesis is whole-utterance and non-cancellable.** `orpheus_tts.py`'s
   `synth()` endpoint returns one complete base64 WAV per call — there is no chunked/streamed response despite
   the original streaming design proposing one, and no cancel RPC exists once `TTS().synth.remote()` is
   invoked (confirmed in `harnessAdapterSurface` and the `AUDIO OUTPUT` capability rows). Correspondingly,
   `converse.py`'s `barge_in` → `tts_cancel` handling only tells the **client** to stop playing audio that has
   already been fully generated (and, on Modal, fully billed) — the opposite of Pipecat/LiveKit-class systems,
   which cancel synthesis **mid-generation** on the server. `latencyMechanics` confirms no `barge_in_min_ms`
   gate or generation-cancellation code exists anywhere in the reviewed streaming/converse code.

Net effect: the harness's only available TTS path today would either (a) eat a 10–60s dead-air pause on the
first turn of most conversations, or (b) require paying for an idle production GPU the harness barely uses, and
either way would only ever be able to *mute playback*, never *stop generating*, when interrupted. None of this
is fixable by touching `orpheus_tts.py`'s config — it requires a different hosting model, a chunked synthesis
protocol, and a real cancel primitive, which is what this PRD specifies.

## 2. User Story

> As Sanskar, when I talk to my voice-driven coding assistant, I want it to start speaking the first sentence
> of its reply almost as soon as the reply starts forming — not after the whole reply is generated — and I
> want it to actually stop generating (not just stop playing) within a beat when I interrupt it. I don't want
> a multi-second silent pause the first time I talk to it after being away for an hour, and I don't want to pay
> for an idle production GPU around the clock just to avoid that pause.

## 3. Objective

Stand up a small, dedicated, **always-on** synthesis process — outside Modal's serverless model entirely —
that the Harness Voice Adapter (the new orchestrator that consumes Orpheus's raw `/v1/stream/transcribe`
turn-taking events per the prior `integrationOptions`/`forkVsCore` research) calls for the output leg of every
harness turn. Concretely:

- **No scale-to-zero.** The process stays warm continuously; the harness never pays a cold-start tax.
- **Chunked, incremental audio.** Text arrives sentence-by-sentence (matching how the harness will stream its
  own reply); audio is returned per-chunk as each sentence finishes synthesizing, so playback of sentence *N*
  starts while sentence *N+1* is still being synthesized — not one blob after the whole reply is done.
- **A genuine cancel primitive.** A `cancel` message actually stops the service from continuing to synthesize
  a turn's remaining chunks, server-side — not just a client-side "stop playing" signal.
- **Additive, not a replacement.** `infra/modal/orpheus_tts.py` is untouched and keeps serving Orpheus's
  product/`ConverseSession` path exactly as today; this is a second, harness-only TTS path.
- **Documented degraded mode.** If this new process is down, the adapter falls back to calling
  `infra/modal/orpheus_tts.py` directly, accepting its cold-start and non-cancellable behavior as a known,
  acceptable degradation rather than a silent failure.

## 4. Functional Requirements

| # | Requirement |
|---|---|
| FR1 | The service exposes a persistent WebSocket endpoint, authenticated by a shared secret distinct from `ORPHEUS_MODAL_SHARED_SECRET` (own secret: `ALFRED_TTS_SHARED_SECRET`, stored in `~/.config/alfred/env`, chmod 600 per repo convention). |
| FR2 | A turn begins with a `{"type":"turn_start","turn_id":"<uuid>","voice":"af_heart","speed":1.0}` control frame. `voice` reuses Kokoro's named-voice convention already established in `orpheus_tts.py:35` (no cloning — out of scope, matching the existing product's own TTS Protocol limits). |
| FR3 | The adapter streams text in **sentence/clause-level units**, one `{"type":"chunk","turn_id","seq":0,"text":"..."}` frame per unit, in order, as its own LLM output produces them — it does not wait for its full reply before sending the first unit. |
| FR4 | For each `chunk`, the service synthesizes that unit's audio and emits exactly one `{"type":"audio","turn_id","seq","pcm_b64","sample_rate":24000,"format":"pcm16"}` reply frame as soon as that unit's audio is ready — before the next unit is requested to start, if the adapter is still producing text, or immediately after if a backlog of `chunk` frames is already queued. Audio is never withheld until the full turn is complete. |
| FR5 | A `{"type":"cancel","turn_id"}` frame immediately (a) drops any `chunk` frames for that `turn_id` still queued and not yet started, and (b) lets the in-flight unit (if any) finish its current synthesis pass but suppresses emitting its `audio` frame and any frames after it. Because Kokoro-82M is not autoregressively streamable at the token level, true mid-*unit* cancellation isn't possible with this model — the mitigation is FR3's sentence-level chunking, which bounds how much audio a single un-cancellable unit can represent (target: units capped at ~1–2s of resulting audio, so the worst-case "can't stop this one" tail is small — see §12). |
| FR6 | A `{"type":"turn_start"}` for a new `turn_id` while a previous turn's chunks are still in flight implicitly cancels the previous turn (same semantics as FR5) — this matches `StreamSession`'s `barge_in` firing on new speech, so the adapter does not need a separate cancel-then-start round trip on barge-in. |
| FR7 | `GET /health` (plain HTTP, not WS) returns `{"status":"ok","model_loaded":true,"uptime_s":...,"active_turn":null|"<uuid>"}` — the signal the adapter and its process supervisor use to decide whether to route to this service or fall back to Modal. |
| FR8 | On any of: WS connect refused, `/health` non-200 or `model_loaded:false`, or no `audio` frame received within a configurable `first_chunk_timeout_ms` (default 800ms) after a `chunk` is sent, the **adapter** (not this service) falls back to calling `infra/modal/orpheus_tts.py`'s `synth` endpoint directly for that turn's remaining text, accepting Modal's cold-start and non-cancellable behavior for that turn only. This fallback logic lives in the Harness Voice Adapter, not in this service. |
| FR9 | Single active turn per connection by design (this is a single-user, single-conversation service — no per-turn concurrency is required); the service does not need a job queue or multi-session scheduler. |
| FR10 | Text/audio content is processed in memory only and is not written to disk beyond ephemeral process logs (§13 sets the logging boundary explicitly). |

## 5. Non-Functional Requirements

- **Always warm.** The model is loaded once at process start and stays resident; no per-request cold path.
- **First-chunk latency.** p50 < 200ms, p95 < 400ms from receiving a `chunk` frame to emitting the
  corresponding `audio` frame, for a typical one-sentence unit (~5–15 words), once warm (see §11 for how this
  budget was derived).
- **Cancellation latency.** From receiving `cancel` to the service guaranteeing no further `audio` frames for
  that `turn_id`: bounded by the in-flight unit's remaining synthesis time, target < 300ms given FR5's
  ~1–2s-of-audio unit cap (i.e., worst case is "the current short sentence finishes, nothing after it plays").
- **Availability posture.** Single point of failure is acceptable *for this personal path* precisely because
  FR8's fallback exists — this is not a highly-available service, it's a fast-path with a documented slow-path.
- **Resource ceiling.** Sized for exactly one concurrent speaker (Sanskar); no horizontal scaling requirement.
- **Cost.** Must be cheap enough that "always on" doesn't reproduce the $792–2,376/month problem this PRD
  exists to avoid — see §10 for the concrete sizing argument for why this is achievable here but wasn't for
  a Modal-hosted A10G.
- **Crash recovery.** Process is supervised (systemd `Restart=on-failure`) and comes back to a warm,
  `model_loaded:true` state within a bounded restart+reload time (target < 15s) without operator intervention.
- **No multi-tenant surface.** This service must be unreachable from Orpheus's product network path — no
  route from `apps/api` or `apps/workers`' `ConverseSession`/`LLMProvider` code, and no organization/session/
  billing concepts (§13).

## 6. Technical Requirements

- **Runtime:** Python, mirroring the existing TTS stack's language choice (`tts.py`, `orpheus_tts.py` are both
  Python/Kokoro) so the same `kokoro` package and voice-name conventions can be reused directly rather than
  re-implemented in another language.
- **Model:** Kokoro-82M, the same model already chosen for `orpheus-tts` (`orpheus_tts.py:35`) — reusing the
  established open-model precedent rather than introducing a new dependency, and small enough (82M params) that
  its resource profile is fundamentally different from the LLM-class workloads Modal's `a10g` sizing targets.
- **Serving loop:** a single `asyncio` event loop per connection; each `chunk` is handled by one bounded
  synthesis call (no internal batching, no request queue beyond the one active turn — FR9 makes this
  unnecessary complexity for a single-user service).
- **Cancellation mechanism:** an `asyncio.Event` (or per-turn cancellation token) checked (a) before starting
  each queued unit's synthesis call and (b) immediately before emitting that unit's `audio` frame — so a
  `cancel` that lands mid-synthesis still suppresses the frame even though the underlying Kokoro call itself
  ran to completion (see FR5's honesty note on why sentence-level chunking, not token-level interruption, is
  the actual cancellation mechanism here).
- **Warm-up:** on process start, run one throwaway synthesis call before flipping `/health`'s
  `model_loaded:true`, mirroring the `@modal.enter` warm-up pattern already used in `orpheus_tts.py` — the
  point is the *first real* request from the adapter is never the one that pays model-load cost.
- **Process supervision:** a `systemd` unit (or equivalent) with `Restart=on-failure`, `RestartSec=2`, running
  on whatever host is chosen per §10.
- **Config:** `ALFRED_TTS_SHARED_SECRET`, `ALFRED_TTS_BIND_ADDR` (default a private/Tailscale interface, never
  a public one — see §13), `ALFRED_TTS_DEFAULT_VOICE`, all read from `~/.config/alfred/env` per repo
  convention, never committed.

## 7. API Requirements

WebSocket, single endpoint `wss://<host>:<port>/v1/synth` (or a local Unix socket / loopback address if
co-located — see §10). Auth: `Authorization: Bearer <ALFRED_TTS_SHARED_SECRET>` on the WS upgrade request,
checked before accepting the connection (reject with HTTP 401 pre-upgrade — same shape as `orpheus_tts.py`'s
in-payload shared-secret check, but on the initial handshake since this is a persistent connection, not a
one-shot POST).

**Client → server frames (JSON, text):**
```jsonc
{"type": "turn_start", "turn_id": "b7e1...", "voice": "af_heart", "speed": 1.0}
{"type": "chunk", "turn_id": "b7e1...", "seq": 0, "text": "Sure, I can help with that."}
{"type": "chunk", "turn_id": "b7e1...", "seq": 1, "text": "Let me check the file first."}
{"type": "turn_end", "turn_id": "b7e1..."}          // no more chunks coming for this turn
{"type": "cancel", "turn_id": "b7e1..."}             // abort remaining/queued chunks for this turn
```

**Server → client frames (JSON, text):**
```jsonc
{"type": "ready"}                                                        // post-auth ack
{"type": "audio", "turn_id": "b7e1...", "seq": 0,
 "pcm_b64": "...", "sample_rate": 24000, "format": "pcm16",
 "duration_s": 1.34}
{"type": "chunk_error", "turn_id": "b7e1...", "seq": 1, "error": "..."}   // this unit failed; others continue
{"type": "cancelled", "turn_id": "b7e1..."}                              // ack that cancel took effect
{"type": "turn_done", "turn_id": "b7e1..."}                              // all chunks for the turn emitted
{"type": "error", "error": "..."}                                        // connection-level error
```

`GET /health` (plain HTTP on the same host:port+1, or a path off the WS server) — see FR7 for the response
shape. No REST session-lifecycle surface (no `/v1/streaming/sessions`-equivalent): this service has no
concept of an org, a billed session, or a persisted transcript, unlike Orpheus's Go relay tier — deliberately,
per §13.

## 8. Architecture

```
                                Same VM (Oracle Cloud ARM, lightweight, no GPU)
   ┌───────────────────────────────────────────────────────────────────────────┐
   │                                                                           │
   │   Mic ──▶ Harness Voice Adapter (new process, separate repo)              │
   │              │        ▲                                                  │
   │              │        │ turn events: partial/final/endpoint/             │
   │              │        │ endpoint_speculative/barge_in/backchannel        │
   │              ▼        │                                                  │
   │      WS client ───────┴────▶  Orpheus worker  /v1/stream/transcribe      │
   │      (ASR leg,                (streaming.py — UNMODIFIED, raw ASR        │
   │       no LLM/TTS                + turn-taking events only, no LLM/TTS    │
   │       opinion)                  opinion baked in)                        │
   │                                                                           │
   │   on `endpoint`: Adapter runs its own harness loop                       │
   │   (Claude Agent SDK — tools, memory, planning; owns its own              │
   │   streaming token output, entirely outside Orpheus)                     │
   │              │                                                           │
   │              │ sentence-level text chunks, streamed as the harness       │
   │              │ produces them (FR3)                                      │
   │              ▼                                                           │
   │      WS client ──────────────▶  THIS SERVICE (§this PRD)                │
   │      (TTS leg, primary)        Streaming Synthesis Service               │
   │              ▲                 - always-on process, Kokoro-82M           │
   │              │ audio chunks,   - private network / loopback only        │
   │              │ per-sentence    - genuine cancel on barge_in              │
   │              │ (FR4)                                                    │
   │              │                                                           │
   │      on barge_in event ──── cancel{turn_id} ─────────────────────────┘   │
   │                                                                           │
   │              │  FALLBACK ONLY (FR8): service unhealthy/unreachable       │
   │              ▼  or first-chunk timeout                                  │
   └──────────────┼────────────────────────────────────────────────────────────┘
                   │
                   ▼
        infra/modal/orpheus_tts.py  (existing, UNMODIFIED)
        Modal-hosted Kokoro, min_containers=0, shared-secret auth
        — accepted degraded mode: cold-start risk, whole-utterance,
          no cancel RPC, but always available as a last resort
```

Note what does *not* change: `ConverseSession`, `LLMProvider`/`llm.py`, and the Go relay/session tier are
untouched — the SaaS product's own voice path keeps calling `infra/modal/orpheus_tts.py` exactly as it does
today. This service and the Harness Voice Adapter sit entirely beside Orpheus, consuming only the one seam
(`/v1/stream/transcribe`) that was already confirmed to need zero Orpheus changes, plus this new component.

## 9. Data Flow

**Normal turn:**
1. Adapter's WS client on `/v1/stream/transcribe` receives an `endpoint` event (session's confirmed
   transcript is ready — same event the ASR engine already emits per `streaming.py`).
2. Adapter starts its own harness turn (tool use, reasoning) and, as its LLM output streams sentences, sends
   `turn_start` then a `chunk` frame to this service per completed sentence — the adapter does not wait for
   its full reply.
3. This service synthesizes each `chunk` in arrival order and streams back one `audio` frame per chunk as soon
   as that sentence's audio is ready.
4. The adapter begins playback of `audio seq:0` as soon as it arrives, while `seq:1` may still be synthesizing
   — this is the actual latency win over `ConverseSession`'s serial `llm.complete()` → `tts.synth()` chain.
5. Adapter sends `turn_end` once its own reply is fully generated; service replies `turn_done` once the last
   queued `audio` frame has been emitted.

**Barge-in turn:**
1. While `audio` frames for `turn_id=A` are still being emitted/played, the ASR leg emits `barge_in` (user
   started talking over the bot — same detection as today's `StreamSession._detect_speech_events`).
2. Adapter immediately (a) stops playback of any buffered audio for `turn_id=A` locally, and (b) sends
   `cancel{turn_id:A}` to this service.
3. This service drops any not-yet-started queued chunks for `A` and suppresses the `audio` frame for any
   in-flight unit, replying `cancelled{turn_id:A}`.
4. The user's new utterance flows through the ASR leg as a new turn; once its `endpoint` fires, step 1 of the
   normal flow repeats with a fresh `turn_id`.

**Fallback path (service down):**
1. Adapter's `chunk` send either fails outright (WS not connected) or the `first_chunk_timeout_ms` budget
   (FR8) elapses with no `audio` frame.
2. Adapter marks this service unhealthy (backed off by a short cooldown, re-checked via `/health`) and, for
   the *remaining* chunks of the in-flight turn, calls `infra/modal/orpheus_tts.py`'s `synth` endpoint
   directly per chunk (or per whole remaining reply, whichever the adapter's simpler code path allows —
   Modal's endpoint has no chunk-cancel primitive either way, so batching remaining text there doesn't make
   the degradation worse).
3. Once `/health` reports `model_loaded:true` again, the adapter routes the *next* turn back to this service.

## 10. Dependencies

- **`infra/modal/orpheus_tts.py`** — unmodified; consumed only as the FR8 fallback. Reuses its existing
  `ORPHEUS_MODAL_SHARED_SECRET`/`ORPHEUS_MODAL_TTS_URL` client pattern from `modal_client.py`, called directly
  by the adapter (not through this service).
- **Orpheus `/v1/stream/transcribe`** (`streaming.py`) — unmodified; the ASR/turn-taking leg this service's
  sibling component (the Harness Voice Adapter) depends on. This PRD does not touch it, but the adapter cannot
  function without it, so it's listed as a system dependency.
- **Harness Voice Adapter** — a co-requisite new component (separate repo, per the `forkVsCore` recommendation
  — not built by this PRD, but this service has no caller without it). This PRD assumes its existence and
  specifies the protocol (§7) the adapter must speak to reach this service.
- **Kokoro-82M** (`kokoro` PyPI package + `espeak-ng` phonemizer dependency, same as `orpheus_tts.py`).
- **A host to run on** — see §11 for the sizing decision; not the Oracle Cloud ARM VM itself (no GPU, and per
  CLAUDE.md, "GPU-heavy work must delegate to Modal or another dedicated GPU environment provisioned later" —
  this service *is* that "another dedicated GPU environment," provisioned specifically because Modal doesn't
  fit this latency/cost profile).
- **Private network path** from the VM to that host if not co-located (Tailscale, matching the "single
  Tailscale hop" framing already used in the `externalPatterns` research to justify skipping WebRTC for this
  deployment — the same reasoning applies here: a controlled, low-loss private link, not the public internet).
- **`~/.config/alfred/env`** for `ALFRED_TTS_SHARED_SECRET` and related config, chmod 600, never committed
  (repo convention).

## 11. GPU Requirements

Kokoro-82M is not comparable in resource profile to Modal's `a10g`-class workloads (that sizing exists for
`orpheus-llm`'s vLLM engine and Whisper large-v3-turbo, both far larger). At 82M parameters, Kokoro is small
enough that CPU inference for short, sentence-level units (the unit size FR3/FR5 already require) is plausibly
sufficient — this must be measured, not assumed, which is why §16's rollout starts with a benchmark milestone
rather than provisioning hardware up front. Two paths, in order of preference:

1. **CPU-first (default assumption until benchmarked).** Run the service on a small, cheap always-on CPU
   instance (or, if proven fast enough, co-located on the harness's own host if that host has spare CPU headroom
   distinct from the lightweight Oracle ARM VM used for the harness itself). Zero GPU spend. Acceptable if the
   §5 first-chunk latency budget (p50 < 200ms) is met for typical one-sentence units — decided by Milestone 0's
   benchmark, not by assumption.
2. **Small dedicated GPU, only if CPU RTF misses the budget.** A single small/cheap GPU instance (e.g., a
   T4/L4-class card, not an A10G) kept warm continuously. Contrast directly with the Modal finding this PRD is
   responding to: keeping a Modal `a10g` warm 24/7 costs ~$792/month for TTS alone (`min_containers=1` math in
   §1). A dedicated small-GPU box for a model two orders of magnitude smaller than an LLM, rented outside
   Modal's per-second-billed serverless pricing, is the kind of always-on cost this PRD's economics actually
   depend on — the sizing decision must confirm a materially lower monthly cost than the Modal warm-pool
   figure before committing to this path, not just assume "smaller model, cheaper GPU" is automatically true.

No `min_containers=0`-style scale-to-zero in either case — that's the entire point of this service existing
(§1, finding 1). No multi-GPU, no autoscaling, no `max_containers` ceiling — this is a fixed, single-instance,
single-tenant deployment (§9's FR9, single active turn per connection).

## 12. Latency Requirements

Grounded in the existing PRD-05 targets and the cross-framework research already done on this thread:

- **Existing product target (unchanged, cited for context):** `docs/prd/prd-05-voice-agent.md` §6 sets
  endpoint→first-TTS-byte < 1.5s (warm Modal LLM+TTS) and barge-in cancel < 200ms (event → TTS stop) for
  Orpheus's own product path — targets that research on this thread found are **asserted, not measured**,
  anywhere in the current codebase (no `time.monotonic()`/`perf_counter()` instrumentation around
  `converse.py`'s ASR→LLM→TTS path, no latency-assertion tests in `test_converse.py`).
- **Cross-framework stage budget (externalPatterns research, LiveKit's own architecture breakdown):** a
  well-streamed cascaded pipeline budgets ~10–50ms VAD, <100ms STT partials, 300–800ms LLM first token,
  **100–200ms TTS first chunk**, totaling ~400–800ms end-to-end; a naive blocking pipeline (full-completion →
  full-synthesis, i.e. today's `ConverseSession` shape) runs 1000–2000ms+; native speech-to-speech models
  (OpenAI Realtime-class) hit ~200–300ms by eliminating the cascade entirely.
- **This service's target, derived from that budget:** the §5 non-functional target of **p50 < 200ms, p95 <
  400ms** for first-chunk latency is set to land this service's contribution inside the "well-streamed
  cascaded pipeline" TTS-first-chunk band (100–200ms) rather than the naive-blocking band, with headroom for
  this being a first implementation on modest hardware.
- **Cancellation target:** < 300ms worst case (§5), tighter than PRD-05's overall 200ms barge-in figure would
  suggest is even possible, but that 200ms figure is for the *whole* event→TTS-stop chain including ASR
  detection and adapter dispatch — this service's own contribution to that chain (queued-chunk drop +
  suppressing one in-flight ~1–2s unit's frame) is the piece under this PRD's control, and is bounded
  specifically by FR5's sentence-level chunk-size cap.
- **Explicit non-goal:** matching OpenAI Realtime's ~200–300ms *end-to-end* figure — that requires an
  audio-native model with no ASR→LLM→TTS cascade at all, which is out of scope; this PRD only removes the
  *TTS* leg's cold-start and blocking-whole-utterance penalties from the cascade Orpheus already has.

## 13. Failure Scenarios

| Scenario | Behavior |
|---|---|
| Service process crashes mid-turn | Adapter's WS read fails; adapter treats this identically to FR8 (unhealthy), finishes the in-flight turn's remaining chunks via Modal fallback; `systemd` restarts the process (target < 15s to `model_loaded:true`); adapter's next `/health` poll picks it back up for the *next* turn. |
| `/health` reports `model_loaded:false` (still warming up after a restart) | Adapter routes new turns to Modal fallback until a subsequent `/health` check reports `true`; no retry storm — adapter backs off health checks on an exponential schedule capped at e.g. 5s. |
| `cancel` arrives after the turn's last `audio` frame already sent | No-op; service replies `cancelled{turn_id}` regardless (idempotent — the adapter doesn't need to reason about the race). |
| Network partition to a remote GPU host (if §11 path 2 is chosen) | Identical handling to a process crash from the adapter's point of view (WS unreachable → FR8 fallback); no special-cased network-vs-process-crash logic needed since the adapter only observes "can't reach it." |
| Malformed `chunk` (empty text, non-UTF8, `seq` out of order) | Service replies `chunk_error{turn_id,seq,error}` for that unit only and continues processing subsequent queued chunks for the same turn — one bad unit does not abort the turn or crash the process. |
| Adapter floods chunks faster than synthesis keeps up | Bounded in-process queue per turn (small, since FR9 means one turn at a time); if the queue depth exceeds a sane cap (e.g. 20 pending sentence units — far more than one reply should ever produce), the service replies `chunk_error` for the overflow unit rather than growing memory unboundedly. |
| Both this service **and** Modal TTS are unavailable | Out of scope for this PRD to "fix" (there is no third fallback) — the adapter must surface an explicit audible/text failure to Sanskar rather than hang silently; specifying that UX is the Harness Voice Adapter's responsibility, not this service's. |
| Voice/model asset missing or fails to load at startup | Process must fail fast and loudly (non-zero exit, clear log line) rather than reporting `model_loaded:true` falsely — `systemd` restart-looping on a genuinely broken install should be visible in `journalctl`, not silently degrade every turn to the Modal fallback indefinitely without anyone noticing. |

## 14. Security Considerations

- **No public exposure.** `ALFRED_TTS_BIND_ADDR` binds to a private interface only (loopback if co-located,
  a Tailscale/private-network address otherwise) — never a public IP. This is a deliberate divergence from
  Orpheus's product services, which must be internet-reachable; this service must not be.
- **Distinct shared secret.** `ALFRED_TTS_SHARED_SECRET` is its own credential, not a reuse of
  `ORPHEUS_MODAL_SHARED_SECRET` — compromising one must not compromise the other, and this service's secret
  never needs to be known by anything inside the Orpheus repo/deployment.
- **No multi-tenancy, and that must stay structurally enforced, not just true today.** Per the `saasCompat`
  research on this thread, the specific failure mode to avoid is a future "just add an org-less bypass" shortcut
  creeping into Orpheus's own auth code to accommodate this service — the mitigation is that this service
  never talks to Orpheus's Go relay/RLS tier at all (§8's architecture diagram has no such edge), so there is
  no code path in `apps/api`/`apps/workers` that could grow a special case for it. A grep for any reference to
  this service's protocol/env vars inside the `orpheus` repo should return zero hits at all times — that's the
  acceptance bar in §17, not just a design intention.
- **Minimal logging.** Per-turn logs may record timing/metrics (§15) and truncated/hashed text previews for
  debugging, but full transcript text and raw audio are not persisted beyond process memory and short-lived
  debug logs with a bounded retention (e.g., 24h local rotation) — this is a personal-use privacy hygiene
  choice, not a compliance requirement, since there is no multi-tenant data-handling obligation here.
- **Fallback secret hygiene.** The Modal fallback path (FR8) reuses the adapter's own existing
  `ORPHEUS_MODAL_SHARED_SECRET`/`ORPHEUS_MODAL_TTS_URL` config, held by the adapter, not by this service — this
  service never needs Modal credentials at all, which keeps its own credential surface to exactly one secret.

## 15. Testing Strategy

- **Unit tests (no model, fake synthesis function injected):** turn lifecycle (`turn_start`→`chunk`×N→
  `turn_end`→`turn_done`); cancellation drops queued-not-started chunks and suppresses the in-flight chunk's
  `audio` frame; a second `turn_start` implicitly cancels the first turn's remaining chunks (FR6);
  out-of-order/duplicate `seq` handling; malformed-chunk handling returns `chunk_error` without killing the
  connection; queue-overflow handling.
- **Integration tests (real model, real WS server, local host):** measure actual first-chunk latency
  distribution for a representative sentence set and assert it against the §5/§11 budget before this service
  is allowed to be the adapter's primary path (a benchmark-gated milestone, §16); verify `/health` transitions
  correctly through cold→warming→ready and back to unhealthy on a forced crash.
- **Fallback integration test (adapter + this service + a stubbed Modal endpoint):** kill this service
  mid-conversation; assert the adapter's next chunk transparently routes to the stubbed Modal endpoint with no
  crash and a logged degradation event; bring the service back; assert the *next new turn* routes back to it.
- **Chaos/soak test:** run the service continuously for ≥ 24h under intermittent synthetic traffic (matching
  the bursty personal-use pattern this PRD is designed around) and confirm zero unplanned restarts, and that
  any planned/forced restart recovers to `model_loaded:true` within the §5 target.
- **Load shape sanity check (not a scale test):** confirm the service behaves correctly under the *actual*
  expected load — one turn at a time, occasional bursts, long idle gaps — rather than testing for concurrency
  it will never see (FR9); this is explicitly not a multi-tenant load test.

## 16. Observability

- **Structured logs**, per-turn: `turn_id`, chunk count, first-chunk latency, total turn synthesis time,
  cancellation events (with the queued-vs-in-flight-suppressed chunk counts), fallback-triggered events (with
  reason: unreachable / unhealthy / timeout).
- **Metrics** (a lightweight local `/metrics` endpoint, mirroring the pattern already established by Orpheus's
  own `control_plane.py` sidecar — a small Prometheus-format endpoint, not a full observability stack, is
  proportionate for a single-user service): `synth_first_chunk_latency_seconds` (histogram), `synth_cancel_
  latency_seconds` (histogram), `synth_turns_total`, `synth_chunks_total`, `synth_cancelled_turns_total`,
  `synth_fallback_triggered_total` (this metric technically lives on the adapter side since it owns the
  fallback decision, but should be co-reported so the two numbers can be compared), `synth_process_uptime_
  seconds`, `synth_model_loaded` (gauge, 0/1).
- **No cost-metering counters** (`tts_gpu_seconds`-style billing fields from Orpheus's product schema are
  explicitly not applicable — there is no org to attribute cost to; §5's cost concern is addressed by the
  hosting-choice decision in §11, not by a metering pipeline).
- **Alerting bar, kept proportionate:** a single check that pages/logs loudly if `synth_model_loaded=0` for
  longer than the expected restart window (§5's < 15s target) — anything more elaborate (dashboards, SLO
  burn-rate alerts) is over-engineering for a one-user service and is explicitly out of scope.

## 17. Rollout Strategy

1. **Milestone 0 — Benchmark (no service built yet).** Run Kokoro-82M locally against representative
   one-sentence text units on both CPU and (if convenient) a candidate small GPU; measure real first-chunk
   latency distributions. This decides §11's CPU-vs-GPU path before any hosting is provisioned — do not
   provision a GPU on the assumption that CPU is insufficient without measuring first.
2. **Milestone 1 — Minimal service, no cancellation.** WS server implementing FR1–FR4, FR7 (turn lifecycle +
   chunked audio + health) on whichever host Milestone 0 selected. Adapter integration is stubbed/manual
   (a test client, not the real Harness Voice Adapter yet, since that component ships separately). Validate
   the §5/§12 latency budget against Milestone 0's numbers under real WS round-trip conditions.
3. **Milestone 2 — Cancellation + fallback.** Add FR5/FR6 (cancel semantics) and the FR8 fallback contract
   (specified here, implemented adapter-side once the adapter exists). Run the chaos/soak test (§15).
4. **Milestone 3 — Real integration with the Harness Voice Adapter.** Once the adapter (separate component)
   exists and drives real conversations, dogfood end-to-end; capture real first-chunk and cancel-latency
   distributions from actual use, not synthetic benchmarks, and compare against §12's targets.
5. **Milestone 4 — Steady state.** No further milestones planned; this is a fixed-scope personal-infra
   component, not a product with a growth roadmap. Revisit only if the harness's usage pattern changes
   materially (e.g., multiple simultaneous conversations, which is explicitly out of scope today per FR9).

No feature flag / gradual-percentage rollout is meaningful here (single user, single deployment) — the
"rollout" is purely the milestone sequence above, gated by the benchmark-then-build order so hosting decisions
are evidence-based rather than assumed.

## 18. Acceptance Criteria

- [ ] Milestone 0 benchmark report exists and explicitly states measured p50/p95 first-chunk synthesis time
      for representative one-sentence units on the chosen hosting path (CPU or GPU), with the choice justified
      against those numbers, not assumed.
- [ ] First audio chunk is emitted within **200ms (p50) / 400ms (p95)** of the corresponding `chunk` frame,
      measured warm, under the soak-test load shape (§15) — not a single hand-picked sample.
- [ ] A `cancel` sent mid-turn results in **no `audio` frames for that `turn_id`** after the currently-in-flight
      unit (bounded by the ~1–2s unit cap), verified by an automated test asserting frame counts before/after
      cancel, not by manual listening.
- [ ] The process has run continuously for **≥ 24h** in a soak test with **zero unplanned restarts** and zero
      cold-start-equivalent latency spikes on any turn during that window.
- [ ] A forced kill of the service mid-conversation results in the **next chunk transparently routing to the
      Modal fallback** (FR8) with no crash, an audible/logged degradation, and the **following new turn**
      routing back to this service once it reports healthy again.
- [ ] `grep -r "ALFRED_TTS\|alfred.*synth\|streaming.synthesis" apps/ infra/ packages/` inside the `orpheus`
      repo returns **zero hits** — confirming no reference to this service exists inside Orpheus's own product
      code (the structural isolation claim in §14).
- [ ] `infra/modal/orpheus_tts.py`'s behavior for Orpheus's own product path (`ConverseSession`) is unchanged
      — verified by Orpheus's existing `test_converse.py`/`test_streaming.py` suites still passing untouched,
      confirming this PRD's "additive, not a replacement" claim (§3) holds in practice.
- [ ] Documented and measured monthly hosting cost for this service is materially lower than the $792/month
      Modal-`min_containers=1`-for-TTS-alone figure from §1/§11 — with an actual dollar figure recorded, not
      just "should be cheaper."
- [ ] End-to-end dogfooding (Milestone 3) produces at least one recorded real conversation where sentence-2
      audio is audibly playing before the harness has finished generating its full reply — the concrete,
      demonstrable version of "chunked, not whole-utterance" that this PRD exists to deliver.
