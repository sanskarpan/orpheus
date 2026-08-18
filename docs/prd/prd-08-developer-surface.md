# PRD: Developer Surface / DX — Drop-in Compatibility, SDKs & MCP

**Status:** Proposed · **Priority:** P1 · **Epic:** Developer Experience · **Related issues:** #327, #445, #303, #444, #330, #447

## 1. Summary

Orpheus today is a well-built but bespoke async platform: submit a job with
`POST /v1/jobs` referencing an uploaded `artifact_id` and a `(processor, version)`
pair, then poll `GET /v1/jobs/{id}` (`apps/api/internal/handlers/jobs.go:129`,
`:414`). That is powerful but unfamiliar — every prospective user must first learn
our upload→job→poll model and hand-roll an HTTP client. This PRD closes the
adoption gap with five DX deliverables, all in scope and all specified to a
production bar (each is delivered complete — the milestones in section 5 are an
ordering of shippable, production-quality increments, not a reduced-scope MVP):

1. An **OpenAI-compatible** (and optionally Deepgram-compatible) synchronous
   transcription endpoint `POST /v1/audio/transcriptions` that maps onto the
   existing upload→job→transcribe flow, so any OpenAI Whisper client works by
   changing only `base_url`.
2. Official **client SDKs** for Python, JavaScript/TypeScript, and Go wrapping the
   native `/v1` surface (uploads, jobs, artifacts, webhooks, streaming tokens).
3. An **MCP server** exposing transcript retrieval and job submission as tools so
   LLM agents can drive Orpheus.
4. A job-create **callback URL** + an **upload-and-poll helper** so simple clients
   never touch the polling loop.
5. A **processor SDK** so third parties can author and register processors that run
   on the worker fleet.

None of this changes core semantics; it is packaging around today's engine. Every
surface here ships production-grade: full error handling with graceful degradation,
bounded concurrency and backpressure, multi-tenant RLS and scope enforcement,
additive backward-compatible on-wire shapes, first-class observability, and exact
per-org cost metering — specified in section 4 and gated in section 6.

## 2. Motivation & goals

**Goals**
- Drop-in migration: an OpenAI Whisper user changes `base_url` + key and it works.
- First-class SDKs remove the boilerplate of multipart presign, part PUTs, and
  poll loops (`apps/api/internal/handlers/uploads.go:3` documents the 3-step flow).
- Agent-native access via MCP for "summarize this recording" style workflows.
- A stable contract for third-party processors mirroring the in-repo processor
  manifest pattern (`apps/workers/src/orpheus_workers/processors/*`).

**Non-goals**
- Real-time OpenAI Realtime API parity (we already have a native streaming WS at
  `handlers/streaming_ws.go:114`; a compat shim over it is a separately-scoped
  milestone, not part of this PRD's deliverables).
- Replacing the native async API — compat endpoints are a convenience layer that sit
  additively alongside the native `/v1` surface.
- Fine-grained per-processor SDK codegen beyond the core resource clients.
- Executing untrusted third-party processor code — the processor SDK ships for
  first-party-reviewed processors at M5; untrusted-code sandboxing is a named later
  milestone (see section 5) with its own production bar, not an open-ended track.

## 3. Current state in Orpheus (file:line, patterns to build on)

- **Job create/poll**: `handlers/jobs.go:129` (`Create`, validates
  `artifact_id`+`processor`, budget hard-cap at `:186`, cache at `:196`, inserts
  `queued` row, returns `202` + `Location`), `handlers/jobs.go:414` (`Get`),
  `:508` (`List`). Response `Job` struct at `jobs.go:70`.
- **Uploads**: presigned multipart, 3 steps documented at `handlers/uploads.go:3`;
  `Create` at `:132`, `Complete` at `:222` (finalizes S3 + inserts `artifacts`
  row in one tx). Content sha256 is what feeds the dedup cache.
- **List envelope**: every list returns `{data, has_more, next_cursor}` via
  `writeList`/`listEnvelope` (`handlers/health.go:57`, `:65`). SDKs standardize on
  this one shape.
- **Auth & scopes**: `X-API-Key` middleware + per-route `RequireScope`
  (`server/server.go:190`, e.g. `rs("jobs:write")` at `:210`, `rs("uploads:write")`
  at `:196`). Compat endpoints reuse these scopes.
- **Transcribe processor**: `apps/workers/src/orpheus_workers/processors/transcribe.py`
  with local CPU vs Modal GPU backend switch (`transcribe.py:34` `_backend()`,
  `:181`); GPU path returns `{text, segments, language, duration_seconds, gpu_seconds}`
  (`infra/modal/orpheus_transcribe.py:136`).
- **Callback precedent**: batches already carry a `callback_webhook_id` fired once
  on completion (`internal/batching/service.go:221`) — per-job callback generalizes
  this pattern.
- **Streaming token**: short-lived HMAC token minting for browser WS
  (`handlers/streaming_ws.go:64` `MintStreamToken`) — SDK streaming helpers wrap it.
- **OpenAPI**: served from `handlers/openapi.json` (`server/server.go:145`) — SDK
  codegen source of truth.

## 4. Proposed design

### 4.1 OpenAI-compatible transcription endpoint (#327, #445)

New handler `handlers/openai_transcriptions.go`, mounted **inside** the `/v1`
group so it inherits authn + rate limit + audit (`server/server.go:172`). Gate with
the existing `rs("jobs:write")` scope (it also needs `uploads:write` internally,
performed under the principal):

```
POST /v1/audio/transcriptions        (multipart/form-data, OpenAI shape)
  file:            <binary>          (required)
  model:           whisper-1 | large-v3-turbo | ...   → mapped to a processor version
  language:        ISO-639-1         → params.language
  prompt:          string            → params.initial_prompt
  response_format: json | verbose_json | text | srt | vtt   (default json)
  temperature:     float             → ignored/best-effort (whisper decode)
  timestamp_granularities[]: word|segment → params.word_timestamps
```

**Mapping onto the existing flow** (synchronous wrapper, no new engine):
1. Stream `file` into the S3 multipart path, reusing the `uploads.go` presign +
   complete internals (or a direct server-side multipart for small files), yielding
   an `artifact_id` + sha256.
2. Resolve `model` → a catalog `(processor.name, version)`. Maintain a small map:
   `whisper-1` → the default transcribe processor version; passthrough for real
   Orpheus model names. Unknown model → `400` OpenAI-style error body.
3. Call the same job-create logic as `jobs.go:129` (artifact ownership check at
   `:176`, budget hard-cap at `:186`, dedup cache at `:196`) — a cache hit returns
   instantly (`serveCacheHit`, `jobs.go:312`).
4. **Block** until terminal using an internal bounded wait; on completion read
   `jobs.result`. The wait is capped by a configurable `sync_wait_deadline` (e.g.
   ~55s, safely under typical client/proxy read timeouts and well under Modal
   `timeout=1800` at `infra/modal/orpheus_transcribe.py:52`). If the deadline is
   reached before terminal state, the endpoint gracefully degrades to the async
   contract: return `202 Accepted` + `Location: /v1/jobs/{id}` (and an
   `OpenAI-Job-Id`/`Retry-After` header) so the caller can poll or receive the
   `callback_url` — the work is never dropped, only handed back for async pickup.
5. Transform `jobs.result` (`{text, segments, language, duration_seconds}`) into the
   requested `response_format`:
   - `json` → `{ "text": ... }`
   - `verbose_json` → `{ task, language, duration, text, segments[] }`
   - `text` → raw string; `srt`/`vtt` → rendered captions (reuse subtitle rendering).

**Response (verbose_json)** mirrors OpenAI exactly so `openai-python`'s
`audio.transcriptions.create()` deserializes unchanged. Errors use OpenAI's
`{ "error": { "message", "type", "code" } }` envelope for compat routes only (the
native surface keeps RFC 7807).

**Error handling & failure modes (graceful degradation).** Every failure path maps
to the OpenAI error envelope so unmodified clients see a well-formed error:
- Unknown/unmapped `model` → `400` with `type: "invalid_request_error"`,
  `code: "model_not_found"`.
- File too large or over the duration cap → `413`/`400` (see scale limits below)
  before any job is created.
- Budget hard-cap tripped at `jobs.go:186` → `402`/`429`-style OpenAI envelope
  (`type: "insufficient_quota"`), no job left half-created.
- Upstream job failure (worker/Modal error, terminal `failed` state) → the job's
  error is surfaced as `{ "error": { message, type: "server_error", code } }` with
  the native `request_id`/`trace_id` echoed in headers for support correlation.
- Sync-wait deadline exceeded → `202` + `Location` fallback (step 4), never a
  hung connection or 5xx.
All error responses carry the same `request_id`/`trace_id` used natively so a compat
failure is traceable end-to-end.

**Scale & concurrency (bounded, backpressured).** The sync endpoint must not let
long files or unbounded in-flight requests exhaust the API tier:
- A per-instance semaphore bounds concurrent blocking sync waits; when saturated the
  endpoint sheds load with `429` + `Retry-After` (backpressure) rather than queuing
  unboundedly.
- Hard `max_file_size` and `max_audio_duration` caps are enforced *before* upload
  completes, rejecting oversized inputs with a typed `400`/`413` so they never pin a
  blocked connection.
- The bounded `sync_wait_deadline` guarantees no request blocks indefinitely; past it
  the request converts to async (202) and frees the connection/goroutine.
- The actual transcription remains fully async on the worker/Modal fleet, so the
  blocking wait consumes only a cheap API-tier goroutine + one poll/notify loop, not
  GPU capacity — the API tier stays responsive under compat load.

**Deepgram-compatible** (same handler family, delivered at milestone M4): `POST
/v1/listen` accepting Deepgram's query params + raw/`url` body, returning Deepgram's
`results.channels[].alternatives[]` shape, and using Deepgram's error envelope on the
compat route (native routes keep RFC 7807). Same internal mapping, same RLS/scope/
metering/observability guarantees as the OpenAI route, and the same bounded sync-wait
+ 202 fallback and size/duration caps. Rolled out behind a per-org feature flag so it
can be enabled progressively, but shipped complete and production-grade — the flag is
a rollout control, not a scope reduction.

**Where it runs:** the compat handler runs in the API tier; the actual transcription
runs exactly where it does today (worker local CPU or Modal GPU per
`ORPHEUS_WORKER_TRANSCRIBE_BACKEND`, `transcribe.py:34`). Metering, budgets, cache,
and webhooks all fire because it *is* a normal job underneath.

**Multi-tenant security & RLS.** Because the handler is mounted inside `/v1`
(`server/server.go:172`) it inherits the `X-API-Key` authn middleware, per-route
`RequireScope` (`rs("jobs:write")` at `server.go:210`, plus `uploads:write`
performed under the same principal), rate limiting, and the audit log — identical to
native routes. The synthesized upload + job rows are stamped with the caller's org,
so the artifact-ownership check at `jobs.go:176` and row-level security apply
unchanged: a compat request can never reference or return another org's artifact or
job (cross-org id → RLS not-found, `jobs.go:414`). Optional PII redaction (PRD 08
redaction) is inherited because the underlying job is a normal job — redaction runs
before `jobs.result` is transformed into the compat response.

**Cost metering.** A compat request is a normal `jobs` row, so metering is identical
to native: `cost_usd` is set at `worker.py:259`, GPU-seconds (`gpu_seconds` from
`infra/modal/orpheus_transcribe.py:136`) and any LLM tokens are attributed to the
caller's org exactly as native jobs, org budgets are enforced at `jobs.go:186`, the
dedup cache short-circuits repeat audio at cost 0 (`serveCacheHit`, `jobs.go:312`),
and completion webhooks fire. There is no separate un-metered path.

**Observability.** Emit compat-specific telemetry alongside native metrics: compat
request latency histogram (labeled by `response_format` and by sync-complete vs.
202-fallback), a counter for sync-wait timeouts/fallbacks, the concurrency-semaphore
saturation gauge (429 shed count), and error counts by OpenAI `error.type`. Every
compat request logs its `request_id`/`trace_id` and the synthesized `job_id` so a
compat call can be joined to its underlying job in logs and dashboards.

### 4.2 Job callback URL + upload-and-poll helper

Extend `CreateJobRequest` (`jobs.go:56`) with an optional `callback_url` (HTTPS).
This is a purely **additive, optional field** — existing clients and the on-wire
request shape are unchanged, and omitting it preserves today's poll/webhook behavior.
On terminal state the worker enqueues an outbox event today (`worker.py:264`); add a
per-job targeted delivery to `callback_url` reusing the batch-callback insert pattern
(`batching/service.go:229`, HMAC-signed like all webhook deliveries).

**Delivery is production-grade, exactly-once-effective, with retries.** The callback
is enqueued through the same durable webhook-delivery path as all other deliveries,
so it inherits: HMAC signing, at-least-once delivery with idempotency (the receiver
dedupes on the delivery id → effectively once), bounded exponential-backoff retries
on non-2xx/timeout, a dead-letter/give-up terminal state after max attempts, and
success/failure metrics. `callback_url` values are validated as HTTPS and screened
against SSRF (no internal/link-local targets). Delivery success rate, retry count,
and give-up count are emitted as metrics per org. This removes the poll loop for
fire-and-forget clients without weakening any delivery guarantee.

The **upload-and-poll helper** is SDK-side sugar (below), not a new endpoint.

### 4.3 Client SDKs — Python / JS / Go (#303, #444)

Generated from `openapi.json` for the resource clients, hand-written ergonomic
helpers on top. Published as `orpheus` (PyPI), `@orpheus/sdk` (npm), and
`github.com/orpheus/orpheus-go`.

Surface (all languages, same nouns):
```
client = Orpheus(api_key=..., base_url=...)
client.uploads.upload(path) -> Artifact         # presign → PUT parts → complete
client.jobs.create(artifact_id, processor="...", version="...", params={...})
client.jobs.wait(job_id, timeout=...) -> Job    # poll GET /v1/jobs/{id}
client.transcribe(path, model="large-v3-turbo") -> Transcript   # upload+job+wait
client.artifacts.signed_url(id) -> str
client.webhooks.create(url, events=[...])
client.streaming.token(session_id) -> str       # wraps MintStreamToken flow
```
Pagination iterators consume `{data, has_more, next_cursor}` (`health.go:57`).
Errors deserialize the RFC 7807 body (`request_id`, `trace_id`) into typed
exceptions. `client.transcribe(...)` is the upload-and-poll helper.

**Production-grade SDK behavior (all three languages).** Resource clients are
generated from `openapi.json` (`server/server.go:145`) **in CI** on every release so
the wire contract can never drift from the server; only ergonomic helpers are
hand-written and unit-tested against the generated types. Each SDK ships:
- Typed error hierarchy mapping HTTP status → exceptions (auth, rate-limit, budget,
  validation, not-found, server), carrying `request_id`/`trace_id` for support.
- Automatic retries with exponential backoff + jitter on `429`/`5xx`, honoring
  `Retry-After`; multipart part PUTs retry per-part; idempotent job-create keys so a
  retried create does not double-bill.
- `client.jobs.wait(...)` uses bounded polling with backoff and a caller timeout, and
  degrades cleanly (raises a typed timeout, job keeps running server-side).
- Streaming helpers reconnect on transient WS drops and refresh the short-lived
  `MintStreamToken` (`handlers/streaming_ws.go:64`) automatically.
- Configurable connection/pool limits and total-request timeouts so an SDK app cannot
  self-DoS; observability hooks expose per-call latency and SDK error rates for the
  host app to record.
Every job created via the SDK is a normal metered job — budgets, dedup cache, and
webhooks all apply, identical to hand-rolled clients.

### 4.4 MCP server for transcript retrieval / agent integration (#330, #447)

A standalone MCP server (`packages/mcp-server`, Node/TS) that authenticates to
Orpheus with a scoped API key and exposes tools:

| MCP tool | Maps to |
|----------|---------|
| `transcribe_audio` | upload + `jobs.create` (transcribe) + `jobs.wait` |
| `get_transcript` | `GET /v1/jobs/{id}` → `result.text`/`segments` |
| `list_jobs` | `GET /v1/jobs` (status/processor filters, `jobs.go:508`) |
| `summarize_transcript` | `jobs.create` with `orpheus.text.summarize` (PRD 04) |
| `search_transcripts` | list + filter over completed job results |

Tools return compact JSON; large transcripts are returned as artifact signed-URLs
rather than inlined. The server is stateless and reuses the Python/TS SDK under the
hood so there is one code path to maintain.

**Production-grade MCP behavior.** The server authenticates with a **scoped** Orpheus
API key (least-privilege: `jobs:read`/`jobs:write`/`uploads:write` only as each tool
requires), so every tool call runs under a real principal and inherits authn, scope
enforcement, rate limiting, audit, and RLS. Cross-org access is impossible: an agent
asking `get_transcript` for another org's `job_id` gets an RLS not-found
(`jobs.go:414`) surfaced as a typed MCP tool error, not a leak. All tool failures are
**typed MCP errors** (invalid-args, not-found, rate-limited, budget-exceeded, upstream
failure) with `request_id`/`trace_id` echoed — never opaque stack traces. Long-running
tools (`transcribe_audio`, `summarize_transcript`) use the SDK's bounded wait with a
caller-visible timeout and degrade to returning a `job_id` the agent can re-check
rather than blocking indefinitely. Signed-URL responses respect artifact ownership.
Underlying jobs are normal metered jobs (budgets, dedup cache, webhooks all fire).
Emit per-tool call counts, latency, and error-rate metrics.

### 4.5 Processor SDK for third parties

Package the in-repo processor contract (`processors/__init__.py` registration,
manifest with `(name, version, model_version_id, cacheable)`) as a public
`orpheus-processor` package: a decorator + `Processor` base exposing `ctx` (db, s3,
work_dir, bucket — the same dict built at `worker.py:181`), input artifact
resolution, and result emission including optional `gpu_seconds` for metering
(`worker.py:250`). Third-party processors register into the catalog via the existing
`sync_catalog` hot-reload (`worker.py:104`, control subject at `worker.py:47`).

**Production error handling & metering for processors.** The `Processor` base defines
a typed result/error contract: a processor either emits a result (with optional
`gpu_seconds` for metering, `worker.py:250`) or raises a typed processor error that
the worker records as a terminal `failed` job with a structured reason — no
unbounded exceptions leak into the job pipeline. Input-artifact resolution enforces
org ownership so a processor only ever sees the calling org's artifacts (RLS-safe).
Manifest validation (`(name, version, model_version_id, cacheable)`) runs at register
time and rejects malformed/duplicate versions; `cacheable` correctness feeds the
dedup cache. Every third-party job is metered exactly like first-party jobs
(`cost_usd` at `worker.py:259`), and per-processor error rate / latency / GPU-seconds
are emitted as metrics.

**Security milestone boundary.** M5 ships the processor SDK for **first-party-reviewed
processors run in the trusted worker context** — this is itself a complete,
production-grade deliverable with the guarantees above. Executing **untrusted**
third-party code is a distinct, named later milestone with its own production bar
(isolation/sandboxing, resource quotas, egress control); it is explicitly out of
scope here (section 2 non-goals) rather than an open-ended track. The M5 boundary is
enforced by first-party review, not by hoping untrusted code behaves.

## 5. Rollout / milestones

Milestones are an **ordering of shippable, production-grade increments** — each one
meets the full production bar of section 4 (error handling, bounded concurrency, RLS/
scope, additive wire shape, observability, exact metering) and section 6's e2e gate
for its surface. None is a reduced-scope prototype; the full feature set is in scope.

1. **M1 — OpenAI compat + callback_url.** Ship `POST /v1/audio/transcriptions`
   (`json`/`verbose_json`/`text`) production-complete: bounded sync-wait + 202
   fallback, size/duration caps, OpenAI error envelope on all failure paths, full
   RLS/scope/metering/observability. Add the additive optional `callback_url` with
   HMAC-signed retrying delivery. Highest adoption leverage.
2. **M2 — Python + JS SDKs** with `transcribe()` upload-and-poll helper, pagination
   iterators, typed errors, retries/backoff, and CI codegen from `openapi.json`; Go
   SDK follows to the same bar.
3. **M3 — MCP server** (`transcribe_audio`, `get_transcript`, `list_jobs`) with scoped
   API key, typed tool errors, RLS isolation, and per-tool metrics.
4. **M4 — Deepgram compat + `srt`/`vtt` formats + `summarize_transcript` MCP**, each
   to the same production bar as its M1/M3 sibling (Deepgram error envelope, caps,
   metering).
5. **M5 — Processor SDK** (docs, template repo, manifest validation) for
   first-party-reviewed processors in the trusted worker context, with typed
   processor error contract and per-processor metering.

Untrusted-processor sandboxing (isolation, resource quotas, egress control) is a
**named later milestone** beyond M5 with its own production bar — see sections 2 and
4.5. It is deliberately sequenced after M5, not descoped into an unbounded track.

## 6. Verification / acceptance criteria

The production bar is **end-to-end**: every criterion below is an automated e2e test
run against a real API tier + a real worker + Modal GPU (`infra/modal/orpheus_transcribe.py`)
in CI/staging — not unit mocks. Each is measurable (pass/fail with concrete
assertions and thresholds).

- **Real drop-in transcription (happy path).** Unmodified `openai-python` with only
  `base_url` swapped calls `audio.transcriptions.create(response_format="verbose_json")`
  against the live endpoint; the file is really transcribed on Modal GPU and the client
  deserializes a `verbose_json` with non-empty `segments[]` (each with `start`/`end`/
  `text`) — asserted end-to-end, no field-shape mismatch.
- **Bounded sync-wait fallback path.** A long audio file that exceeds the
  `sync_wait_deadline` returns `202` + `Location: /v1/jobs/{id}` (not a hung
  connection or 5xx) within the deadline; polling that `Location` (or the fired
  `callback_url`) subsequently yields the completed transcript. Assert the connection
  is released at ~deadline and the same job completes async.
- **Metering & budget & dedup (real job underneath).** A compat request produces a
  normal `jobs` row visible in `GET /v1/jobs`, metered with `cost_usd > 0`
  (`worker.py:259`) and GPU-seconds attributed to the org; it is rejected when it would
  breach the budget hard-cap (`jobs.go:186`) with an OpenAI `insufficient_quota`
  envelope; and a **repeat of the same audio returns from `serveCacheHit`
  (`jobs.go:312`) at `cost_usd == 0`** — asserted by comparing billed cost on first vs.
  second call.
- **Callback delivery (exactly-once-effective, signed, retried).** `callback_url`
  fires exactly once on terminal state with a valid HMAC signature; a test receiver
  that returns `500` on first attempt observes a backoff **retry** and eventual
  success, and the delivery-id dedup guarantees the effective payload is processed
  once. Assert retry count and give-up-after-max behavior.
- **Negative / failure paths (OpenAI envelope).** An unknown `model` returns `400`
  with `{ error: { type: "invalid_request_error", code: "model_not_found" } }`; a
  forced upstream/worker failure surfaces as an `error` envelope with `type:
  "server_error"` and an echoed `request_id`/`trace_id` — never a raw stack trace or
  RFC 7807 body on the compat route.
- **Multi-tenant isolation (RLS).** Org A's key calling MCP `get_transcript` (or the
  compat/native routes) for Org B's `job_id`/`artifact_id` gets an RLS **not-found**
  (`jobs.go:414`) as a typed error, and cannot retrieve Org B's transcript, signed
  URL, or job listing — asserted with two real orgs end-to-end.
- **SDK ergonomics & pagination.** SDK `client.transcribe(path)` round-trips a real
  file in ≤5 lines; the pagination iterator walks multiple `next_cursor` pages over a
  seeded set; SDK types are regenerated from `openapi.json` in CI and a drift check
  fails the build if the committed client diverges from the served schema.
- **Envelope separation.** Compat routes (OpenAI + Deepgram) return their respective
  error envelopes while native `/v1` routes keep RFC 7807 — asserted by hitting both
  with the same failure and diffing the bodies.
- **Observability.** After an e2e run, assert the compat latency histogram,
  sync-wait-timeout counter, callback delivery success/retry counters, MCP per-tool
  call counters, and SDK error-rate signals are populated with the expected labels.

## 7. Dependencies, risks, open questions

- **Sync-over-async latency**: a long file blocks the HTTP request. Resolved (not
  merely mitigated) at M1 by the bounded `sync_wait_deadline` + `202`-with-`Location`
  fallback (section 4.1), enforced `max_file_size`/`max_audio_duration` caps, and a
  concurrency semaphore with `429` backpressure so no unbounded set of in-flight sync
  requests can pin the API tier. The max duration and size caps are documented in the
  compat endpoint reference.
- **Model-name mapping** must stay stable; a `whisper-1` alias needs a pinned
  default processor version so results are reproducible/cacheable.
- **PII / data egress**: compat endpoint inherits the same tenant isolation, RLS,
  audit log, and optional redaction (PRD 08 redaction) because it is mounted inside
  `/v1` and runs a normal job underneath (section 4.1) — compat traffic is auditable
  by construction and gated by the multi-tenant isolation e2e test in section 6.
- **SDK maintenance**: generate resource clients from `openapi.json` in CI to avoid
  drift; only helpers are hand-written.
- **Open**: do we expose word-level timestamps in `verbose_json` by default (cost of
  `word_timestamps=true`, `infra/modal/orpheus_transcribe.py:90`)? Deepgram compat
  priority vs. OpenAI Realtime shim priority?

## 8. Effort

- OpenAI compat endpoint + `callback_url`: ~1.5 weeks (1 backend eng).
- Python + JS SDKs (codegen + helpers + tests + publish): ~3 weeks.
- Go SDK: ~1.5 weeks.
- MCP server: ~1.5 weeks.
- Deepgram compat + caption formats: ~1 week.
- Processor SDK (packaging, template, docs): ~2 weeks.
- **Total: ~1 quarter** for 1–2 engineers, shippable incrementally per milestone
  (M1–M5), each delivered to the production bar in sections 4 and 6 rather than as a
  reduced-scope MVP. Estimates cover the production hardening (error paths, bounded
  concurrency, RLS/scope, observability, e2e tests), not just the happy path.
