# Design 12 — WebRTC ingress + streaming ASR + WebSocket API

> **Status:** proposed · **Owner:** platform / ML platform · **Scope:** gap #12
> **Depends on:** [PRODUCTION_DESIGN §2.6 (opportunity 3), §3.8, §8.1](../architecture/PRODUCTION_DESIGN.md), [design #10 (GPU plane)](10-gpu-inference.md)
> **Positioning:** this is the **Deepgram-style extension** the main design
> explicitly defers to v2 (§1.2, §2.6). It is *additive* — it does not change the
> async-first core.

Design + build checklist. No code is added by this design; it is the last of the
four future-subsystem docs (gaps #9–#12) and the one furthest from the current
architecture.

---

## 1. Problem

Orpheus is **async-first by design** (§1.2): upload → job → webhook/poll. That
is correct for batch audio and is where the product earns its place. But there
is a real, upmarket use case it cannot serve today:

- **Live transcription** — captions for a live stream, an agent-assist tool on a
  phone call, real-time meeting notes. The user wants **partial transcripts as
  they speak**, with sub-second latency, over a **persistent connection**, not a
  file upload and a webhook minutes later.

The current architecture has no answer for this:

| Need | Async core today | Streaming needs |
|---|---|---|
| Transport | HTTP request/response, presigned S3 upload | bidirectional, low-latency audio ingress (WebRTC) |
| ASR | whole-file whisper (or chunked via #9) | **streaming** ASR emitting partials incrementally |
| Result delivery | webhook / poll / SSE (one-way) | **WebSocket** (bidirectional, interim + final) |
| SLA | seconds–minutes | **first-partial < ~500 ms**, steady p95 latency |
| Session | stateless request | long-lived, stateful, reconnectable session |

`§8.1` already flags WebSocket as "deferred until we add real-time
transcription (v2)" and `§3.8` marks WebRTC/WebSockets as v2. This design is that
v2 subsystem.

---

## 2. Proposed architecture

```
   Client (browser/SDK)
     │  1. POST /v1/streaming/sessions  → session token + ICE/SFU coordinates
     │     (auth: API key / JWT; session pinned to org via RLS)
     │
     │  2a. WebRTC (audio media) ─────────────► Media ingress (SFU / WebRTC
     │        Opus/PCM, DTLS-SRTP, jitter buffer   gateway; e.g. Pion or
     │                                             mediasoup-class)
     │
     │  2b. WebSocket (control + results) ◄────► Streaming API gateway
     │        {partial}/{final} transcript,       (Go; bidirectional)
     │        {speaker}, {endpoint}, backpressure
     ▼
   ┌──────────────────────────────────────────────────────────────┐
   │ Streaming session service                                     │
   │  - owns the session state machine (connecting→live→closing)   │
   │  - decodes/resamples media → 16 kHz mono frames               │
   │  - feeds a STREAMING ASR model replica (sticky per session)   │
   │  - emits interim + final transcripts over the WebSocket       │
   │  - optional streaming diarization / endpointing (VAD)         │
   └───────────────┬──────────────────────────────────────────────┘
                   │ sticky routing (one session ↔ one GPU replica)
                   ▼
   ┌──────────────────────────────────────────────────────────────┐
   │ Streaming ASR on the GPU plane (design #10)                   │
   │  - a STREAMING-capable model (e.g. streaming whisper /        │
   │    a chunk-with-lookahead decoder), NOT batch whole-file      │
   │  - stateful per session (rolling context), low-latency,       │
   │    NOT dynamic-batched the way offline whisper is             │
   └──────────────────────────────────────────────────────────────┘

   On session end: finalize a Result (same shape as batch, §7.2) and persist,
   so streaming and batch share the result/model_version/cost model.
```

### 2.1 WebRTC ingress

- **Why WebRTC, not raw WS audio:** WebRTC gives Opus, congestion control,
  jitter buffering, packet-loss concealment, and NAT traversal (ICE/STUN/TURN)
  out of the box — essential over real networks (mobile, lossy Wi-Fi). Raw audio
  over WebSocket is simpler but has none of that; we keep WS for **control +
  results** and use WebRTC for **media**.
- **Ingress** is an SFU/WebRTC gateway (a Go-friendly stack such as **Pion**,
  keeping the streaming edge in Go alongside the API tier). It terminates
  DTLS-SRTP, dejitters, and hands PCM frames to the session service.
- **Sticky sessions:** a session is pinned to one ingress instance and one GPU
  ASR replica for its lifetime (stateful ASR context). Routing is
  session-token-based; reconnect re-attaches to the same session if within a
  grace window, else starts fresh.

### 2.2 Streaming ASR

- Requires a **streaming-capable decoder**, distinct from offline whisper:
  chunk-with-lookahead or a purpose-built streaming model, emitting **interim
  (unstable) partials** frequently and **finalized segments** on endpoints
  (VAD-detected pauses).
- **Stateful, sticky, not batched** the way offline whisper is (§10.2 dynamic
  batching is for many independent chunks; a live session is one continuous
  stream). It runs on the same GPU plane (design #10) but as a distinct Ray
  Serve deployment with per-session state and low-latency scheduling.
- **Model versioning still applies** (ADR-0005): the streaming model is a
  `model_version`; the finalized `Result` records it, so a session is as
  reproducible as batch (given the same audio).
- **Optional:** streaming VAD/endpointing, streaming diarization (coarser than
  the offline pyannote path in #10), interim punctuation.

### 2.3 WebSocket API (control + results)

- Bidirectional JSON (or protobuf) messages: client → server `start`,
  `keepalive`, `finalize`, `close`; server → client `partial`, `final`,
  `speaker`, `endpoint`, `error`, `metrics`.
- **Backpressure:** if the client can't keep up, the server drops interim
  partials (never finals) and signals congestion. Flow control on both media and
  result channels.
- **Auth:** the WS/WebRTC session is authorized by the short-lived session token
  from `POST /v1/streaming/sessions`; the token carries `org_id` so results and
  cost are RLS-scoped exactly like batch.

### 2.4 Fit with the current architecture

- **Additive, not a rewrite.** The batch core (uploads, jobs, webhooks, #9
  Temporal, #10 GPU plane) is untouched. Streaming adds a *new edge* (WebRTC/WS)
  and a *new ASR deployment*, reusing: the GPU plane (#10), the model registry
  (#10), the result/cost model (§7.2), RLS tenancy, and billing (#11).
- **Different transport, same domain.** A finished session persists a normal
  `Result` (`model_version_id`, cost, segments), so it shows up in the dashboard
  (#11), is metered/billed (#11), and is reproducible (ADR-0005).
- **Separate deployment + scaling.** Streaming pods are latency-SLA, sticky,
  stateful — a different operational profile from stateless API pods and
  throughput-oriented batch workers. They get their own node pool and autoscaler
  keyed on active-session count + GPU utilization, not RPS or queue depth.

---

## 3. Data-model changes

Additive (new migration `0008_streaming.sql`; **out of scope here — do not edit
existing migrations**):

```sql
CREATE TABLE streaming_sessions (
  id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id             uuid NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
  model_version_id   uuid,                 -- streaming ASR model pinned (ADR-0005)
  status             text NOT NULL DEFAULT 'connecting',  -- connecting|live|closing|closed|failed
  started_at         timestamptz NOT NULL DEFAULT now(),
  ended_at           timestamptz,
  audio_seconds      numeric,              -- billable duration
  result_id          uuid,                 -- finalized Result at session end
  ingress_node       text,
  error              text
);
-- RLS: same policy shape as workflows (tenant_select/insert/update/delete on org_id).
```

- Live interim transcripts are **ephemeral** (not persisted per-partial); only
  the finalized `Result` is stored (reuse the existing result path).
- `audio_seconds` feeds usage metering (#11) — streaming is billed per second of
  audio processed.

---

## 4. API surface

New, alongside the existing REST surface (§8.3):

```
POST /v1/streaming/sessions        # create session → session token + ICE/SFU coords
GET  /v1/streaming/sessions/{id}   # status + finalized result_id
DELETE /v1/streaming/sessions/{id} # tear down

# non-HTTP endpoints (authorized by the session token):
WS   /v1/streaming/{session_id}    # control + interim/final transcripts
WebRTC (SDP/ICE)                    # audio media ingress via the SFU
```

REST stays the primary developer surface (§8.1); streaming is opt-in for the
real-time use case.

---

## 5. Rollout plan

1. **Spike the ASR.** Prove a streaming-capable model hits the first-partial
   latency SLA on the GPU plane (#10), offline, no transport. Kill-criterion: if
   latency/quality is unacceptable, stop here.
2. **WS results, file-fed.** Ship the WebSocket result protocol fed by a
   simulated stream (chunked file) — validate the message protocol,
   backpressure, and finalize→Result path without WebRTC complexity.
3. **WebRTC ingress.** Add the SFU/Pion gateway, DTLS-SRTP, jitter buffer,
   sticky routing. Internal dogfood.
4. **Private beta.** A few design-partner tenants; measure real-network latency,
   packet loss, reconnect behavior; tune autoscaling.
5. **GA (opt-in).** Metered/billed via #11; dashboard shows sessions; SLA
   published.

Every step is behind a flag and additive; the batch product is never at risk.

---

## 6. Latency SLAs (targets)

| Metric | Target |
|---|---|
| First interim partial after speech onset | < 500 ms p95 |
| Interim update cadence | ≤ 300 ms |
| Final segment latency after endpoint (VAD pause) | < 1.0 s p95 |
| End-to-end audio→WS glass-to-glass | < 800 ms p95 on good networks |
| Session setup (POST → WS/WebRTC live) | < 1.5 s p95 |
| Reconnect grace window | ~10 s (re-attach to same session) |

These are **SLAs the streaming path is designed to**, not the batch SLOs (§12 of
the main design). They drive the sticky, stateful, non-batched ASR choice.

---

## 7. Risks

| Risk | Likelihood | Mitigation |
|---|---|---|
| Latency SLA unachievable with available streaming models | Med–High | Spike first (rollout step 1) as a kill-gate before building transport. |
| WebRTC operational complexity (TURN, NAT, SFU scaling) | High | Use a mature stack (Pion); TURN via managed/coturn; start with a single-region SFU; treat streaming as its own node pool. |
| Sticky sessions hurt autoscaling / bin-packing | Med | Session-aware autoscaler; drain-before-scale-down; short reconnect grace so lost pods don't strand sessions. |
| GPU cost per concurrent session (no batching) | High | Streaming is a premium tier; per-second metering (#11); cap concurrent sessions per tenant (bulkhead); scale-to-zero when idle. |
| Security: long-lived media/WS connections widen attack surface | Med | Short-lived session tokens, DTLS-SRTP, org-scoped RLS, per-session resource + duration caps, no egress from ASR pods (gVisor, ADR-0008). |
| Divergence from async-first product focus | Med | Explicitly v2 / opt-in / premium; batch remains the core (§1.2); reuse (registry, results, billing) keeps it from being a parallel product. |
| Partial/final transcript instability confuses clients | Med | Clear interim-vs-final semantics in the WS protocol + SDK; never revise a `final`. |

---

## 8. What must be built (checklist)

- [ ] **Kill-gate spike:** a streaming-capable ASR model on the GPU plane (#10)
      meeting the first-partial latency SLA offline.
- [ ] WebSocket API: bidirectional protocol (`start`/`keepalive`/`finalize`/
      `close` ↔ `partial`/`final`/`speaker`/`endpoint`/`error`/`metrics`), with
      backpressure (drop interims, never finals).
- [ ] WebRTC ingress (SFU/Pion): DTLS-SRTP, jitter buffer, Opus decode → 16 kHz
      mono frames; ICE/STUN/TURN.
- [ ] Streaming session service: session state machine, sticky routing (session
      ↔ GPU replica), reconnect grace window.
- [ ] Streaming ASR Ray Serve deployment (stateful, per-session, low-latency,
      non-batched); VAD/endpointing; optional streaming diarization.
- [ ] `POST/GET/DELETE /v1/streaming/sessions` + session-token auth (org-scoped).
- [ ] Migration `0008_streaming.sql` (`streaming_sessions` + RLS). **Do not edit
      existing migrations.**
- [ ] Finalize path: session end → persist a normal `Result` (§7.2) with
      `model_version_id` + cost; feeds dashboard (#11) and metering (#11).
- [ ] Dedicated node pool + session-aware autoscaler (active-session count);
      per-tenant concurrent-session bulkhead; scale-to-zero when idle.
- [ ] gVisor + no-egress on ASR pods; short-lived session tokens; per-session
      duration/resource caps.
- [ ] SDK support (browser + server) for the WebRTC/WS session lifecycle.
- [ ] Load/latency test harness under real-network conditions (loss, jitter).
