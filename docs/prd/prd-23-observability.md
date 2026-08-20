# PRD: Observability & Ops

**Status:** Proposed · **Priority:** P1 · **Epic:** Observability & Ops · **Related issues:** #A6

## 1. Summary

Orpheus has the primitives of an observability stack — a per-instance Prometheus registry, HTTP
and outbox metrics, an hourly usage/cost rollup, and a JetStream queue-depth gauge already
exported by the worker — but they don't yet answer the questions operators and customers actually
ask: *which model/processor is slow, what is a GPU-second really costing us, are we meeting our
SLA, and should we scale up right now?* GPU work runs on Modal, which exposes GPU-utilization
metrics and already returns `gpu_seconds` per call, yet none of that flows into a dashboard or
cost view. This PRD builds per-model/per-processor latency+throughput+GPU metrics, cost dashboards
tied to real spend, SLA/uptime instrumentation, an autoscaling consumer for the queue-depth
signal, and closes three specific test gaps (#A6): the streaming relay path, `ListDeliveries`
cursor/status edges, and a GPU load/soak test.

## 2. Motivation & goals

- **Per-model/per-processor performance:** today only `orpheus_jobs_submitted_total{processor}`
  exists on the API side and a single ack-latency histogram on the worker side. There is no
  latency/throughput broken out by model, and GPU utilization is invisible to us despite Modal
  measuring it.
- **Real cost:** the usage rollup aggregates `cost_usd` per hour/processor, but nothing ties it
  to actual GPU spend (`gpu_seconds` × Modal GPU rate) or surfaces it as a dashboard.
- **SLA/uptime:** there is no uptime SLI, no error-budget, no per-processor success-rate SLO.
- **Autoscaling:** the worker publishes `orpheus_jetstream_pending_messages` but nothing consumes
  it to drive scaling decisions.
- **Test gaps (#A6):** the streaming relay, delivery-list pagination edges, and GPU behavior under
  load are the least-covered, highest-risk paths.
- **Non-goals:** replacing Prometheus/Grafana; distributed tracing beyond the existing
  `observability/tracing.go`; building a billing engine (usage rollup already owns cost math).

## 3. Current state in Orpheus

- **API metrics:** `internal/metrics/metrics.go` — per-instance registry (`New`, `:29`) with
  `HTTPRequests`, `HTTPDuration`, `JobsSubmitted{processor}` (`:53`), `OutboxPublished`,
  `OutboxPublishLatency`, `RLSDenials`. Served at `/metrics` from the per-instance registry
  (`server/server.go:139`). No GPU, model, or per-processor latency collectors.
- **Worker metrics:** `apps/workers/src/orpheus_workers/metrics.py` — `JETSTREAM_PENDING` gauge
  (`:11`, `orpheus_jetstream_pending_messages`), a processor-labeled ack-latency histogram (`:24`),
  and `orpheus_jetstream_messages_total{result}` (`:31`). Queue depth is polled every
  `queue_depth_poll_seconds` (default 15s, `config.py:29`) via `record_queue_depth`
  (`worker.py:52`) → `js.consumer_info(...).num_pending` (`:59-63`), started as a background task
  (`worker.py:111`, `_poll_queue_depth` `:123`).
- **GPU / Modal:** `infra/modal/orpheus_transcribe.py` returns `gpu_seconds` (`:133`, `:142`) on
  each call; `orpheus_diarize.py` returns it per branch (`:107`, `:123`, `:169`); `orpheus_llm.py`
  too. GPU type is `a10g` (`:47`). Modal itself exposes container GPU-utilization metrics we don't
  scrape. The worker records `gpu_seconds` into job cost but no dimension survives to metrics.
- **Usage/cost rollup:** `internal/usage/service.go` — `RollupOnce` (`:70`) upserts
  `usage_rollup_hourly` across `total` / `processor` / `status` dimensions, summing
  `compute_seconds` (from `completed_at-started_at`, `:79`) and `cost_usd` (`:83`). `BudgetSpend`
  (`:175`) reads current-period spend. This is the cost backbone to build dashboards on.
- **Streaming relay:** `handlers/streaming_ws.go` — `StreamTranscribe` (`:114`) dials the worker
  WS and pumps frames both directions (`relay`, `:169`), metering PCM bytes for billing (`:196`).
  There is a `streaming_ws_test.go` (token) and `streaming_integration_test.go`, but no end-to-end
  relay test that stands up a fake worker WS and asserts byte-metering + status transitions.
- **ListDeliveries:** `handlers/webhooks.go:419` — cursor pagination (`created_at`-based,
  `nextCursor` at `:110` region) + `status`/`event_type` filters with `validDeliveryStatus`
  validation. Edge cases (empty cursor, last page, invalid status, filter+cursor combo) are
  lightly tested.

## 4. Proposed design

**4.1 Per-model/per-processor performance metrics.** Add collectors to
`internal/metrics/metrics.go`:
- `orpheus_job_duration_seconds{processor, model, status}` (histogram) — recorded when a job
  completes, from the same `completed_at-started_at` the rollup uses.
- `orpheus_job_throughput` derivable from job counters already present; add
  `orpheus_jobs_completed_total{processor, model, status}`.
On the worker, extend `metrics.py` with `orpheus_gpu_seconds_total{processor, model, gpu_type}`
(counter) and `orpheus_audio_seconds_total{processor}` fed by the `gpu_seconds`/`duration_seconds`
already returned from Modal (`orpheus_transcribe.py:142`). Model name comes from the processor
params (`params->'_processor'->>'name'` is already the rollup's processor key,
`usage/service.go:85`).

**4.1 (cardinality & failure).** The `model` label is bounded to the known registry model set (an
allow-list), and any unrecognized value collapses to `other`, so a caller cannot explode Prometheus
cardinality with arbitrary model strings. Recording a metric never fails the job path — collector
errors are swallowed and counted, not propagated. Worker metrics remain per-instance (the existing
registry model), aggregated at scrape time, so a single worker restart loses only its in-memory
counters, not the rollup (which is DB-backed, `usage/service.go`).

**4.2 GPU-utilization metrics.** Scrape Modal's GPU metrics: add a small exporter (a scheduled
Modal function or the worker) that queries the Modal metrics API per app
(`orpheus-transcribe`, `orpheus-diarize`, `orpheus-llm`) and republishes
`orpheus_modal_gpu_utilization{app, gpu_type}` and container-count/cold-start gauges into the
Prometheus registry. Pair GPU utilization with `gpu_seconds` to compute effective GPU efficiency
(billable audio-seconds per GPU-second).
**Failure modes & degradation.** If the Modal metrics API is unreachable, rate-limits, or the
token is invalid, the exporter serves the last-scraped value with a staleness gauge
(`orpheus_modal_metrics_scrape_age_seconds`) and increments
`orpheus_modal_metrics_scrape_errors_total` rather than emitting zeros (which would falsely read as
"GPUs idle" and could mislead the autoscaler). Scrape runs on a bounded interval with a timeout so
a slow Modal API cannot back up the exporter. The Modal API token is resolved from the secrets
provider (see prd-11), never a committed default, and read-only.

**4.3 Cost dashboards tied to real spend.** Build Grafana dashboards over `usage_rollup_hourly`:
spend by processor/model/hour, GPU-second cost (`orpheus_gpu_seconds_total` × Modal GPU $/s),
cost-per-audio-minute, and budget-utilization (reusing `BudgetSpend`, `service.go:175`). Add a
`GET /v1/usage/cost` breakdown that returns the same dimensions the rollup stores, so the
dashboard and the tenant API agree. Reconcile platform GPU spend (Modal invoice) against summed
`gpu_seconds` monthly to catch drift.
**Multi-tenant security.** `GET /v1/usage/cost` is org-scoped under FORCE-RLS and gated by a
`usage:read` scope, so a tenant sees only its own spend; the platform-wide reconciliation view is
`platform:admin`-only. **Metering correctness** depends on `gpu_seconds` being present on every
return path — the diarize service has three (`orpheus_diarize.py:107/:123/:169`), all of which must
carry it; a missing `gpu_seconds` is treated as a metering defect (alert), not silently costed at
zero. **Backward-compatible on-wire:** `GET /v1/usage/cost` is additive and reuses the existing
rollup dimensions, so no change to current usage endpoints.

**4.4 SLA/uptime instrumentation.** Define SLIs from existing collectors:
availability (`orpheus_http_requests_total` non-5xx ratio), per-processor success rate
(`orpheus_jobs_completed_total{status}`), and streaming session success. Add a synthetic
liveness probe hitting `/api/live`+`/api/ready` (`server.go:135` area) recorded as
`orpheus_uptime_probe`. Publish an error-budget dashboard and burn-rate alerts. Expose a
public/tenant status derived from these, not hand-maintained.

**4.5 Consume the autoscaling signal.** Add an autoscaler that reads
`orpheus_jetstream_pending_messages` (`metrics.py:11`) and the job-duration histogram to compute
desired worker/GPU concurrency. For Modal, the transcribe app already scales-to-zero with
`min_containers=0` (`orpheus_transcribe.py`); the signal drives a warm-pool floor
(prewarm — cf. `handlers/prewarm.go`) when queue depth rises, avoiding cold-start latency during
bursts. Emit `orpheus_autoscale_desired_replicas` for observability and start with a
KEDA-style scaled-object config (queue-depth threshold → replica count) rather than custom
control logic.
**Bounded cost & failure modes.** Desired replicas are clamped to a hard `[min, max]` band so the
warm-pool floor can never provision unbounded GPU, and scaling is rate-limited with hysteresis so a
flapping queue does not thrash containers. Because Modal GPU metrics can lag (§4.2), the scaler
uses queue depth (real-time) as the primary signal and in-flight duration as a secondary, and holds
last-known-good if the signal is stale rather than scaling on missing data. This shares the
autoscaler contract with prd-10 §4.2 — the same warm-pool floor and guardrails, viewed here from
the observability/signal side.

**4.6 Test gaps (#A6).**
- **Streaming relay integration test:** stand up a fake worker WebSocket, drive
  `StreamTranscribe` (`streaming_ws.go:114`) with PCM frames, assert bidirectional relay,
  server-side byte metering (`:196`) sets `audio_seconds`, and status transitions
  live→closing (`setStreamStatus`, `:219`) including the worker-outage 502 path (`:148`).
- **ListDeliveries cursor/status edges:** table-driven test over `webhooks.go:419` — empty
  cursor first page, `nextCursor` round-trip to exact last page, invalid `status` → 400
  (`validDeliveryStatus`), `status`+`event_type`+`cursor` combined, and the historical bug where
  the LIMIT placeholder collided with the org param (guard against regression).
- **GPU load/soak test:** a load harness driving the Modal transcribe/diarize endpoints at
  concurrency (`@modal.concurrent(max_inputs=4)`, `orpheus_transcribe.py`) measuring cold-start
  vs warm latency, `gpu_seconds` accuracy, and memory stability over a multi-hour soak; assert
  no leak and bounded p99.

## 5. Rollout / milestones

Ordering only — each milestone is a production-quality, shippable increment (bounded cardinality,
failure handling, org-scoped access), not a reduced-scope prototype. The full §4 scope is committed.

1. **M1 — Metrics foundation:** 4.1 collectors (API + worker) with bounded `model` cardinality and
   swallow-and-count collector errors, plus 4.6 delivery/relay tests. Fast, unblocks everything.
2. **M2 — GPU + cost:** 4.2 Modal exporter with staleness/last-known-good on scrape failure,
   4.3 cost dashboards + org-scoped `GET /v1/usage/cost` (RLS + `usage:read`) + monthly invoice
   reconcile with documented tolerance.
3. **M3 — SLA:** 4.4 SLIs, error-budget + burn-rate dashboards, synthetic `/api/live`+`/api/ready`
   probes, tenant status derived (not hand-maintained).
4. **M4 — Autoscaling:** 4.5 consume queue-depth signal with a clamped warm-pool floor, hysteresis,
   stale-signal hold, then close-loop scaling. Shares the prd-10 §4.2 guardrails.
5. **M5 — Load/soak:** 4.6 GPU soak test in CI-nightly asserting no leak and bounded p99.

## 6. Verification / acceptance criteria

End-to-end against a running API + worker + the live Modal apps and a real Prometheus/Grafana (not
unit-only). Each item covers a positive path, a negative/failure path, and multi-tenant scoping
where relevant.

- **Metrics (labels + cardinality + safety):** `/metrics` exposes `orpheus_job_duration_seconds`
  and `orpheus_jobs_completed_total` labeled by processor+model+status; the worker exposes
  `orpheus_gpu_seconds_total`. An unknown model value collapses to `other` (cardinality bounded);
  a forced collector error increments the error counter without failing the job.
- **GPU exporter (scrape + failure):** `orpheus_modal_gpu_utilization` is scraped for all three
  Modal apps and visible in Grafana; simulating a Modal metrics-API outage serves last-known-good,
  bumps `orpheus_modal_metrics_scrape_age_seconds`, and increments the scrape-error counter (never
  false-zero).
- **Cost (reconcile + isolation):** cost dashboard totals reconcile with the Modal invoice within a
  documented tolerance, and summed `gpu_seconds` × rate matches `usage_rollup_hourly.cost_usd` for
  GPU processors; a job on the diarize path exercising each of the three return points
  (`:107/:123/:169`) always carries `gpu_seconds`; `GET /v1/usage/cost` returns only the calling
  org's spend (RLS-scoped) and rejects a caller lacking `usage:read`.
- **SLA:** the SLO dashboard shows availability + per-processor success rate with a working
  error-budget burn-rate alert that fires on an injected error spike; synthetic probes record
  `orpheus_uptime_probe`.
- **Autoscaler (scale + bound + stale-signal):** the warm pool scales up when
  `orpheus_jetstream_pending_messages` exceeds threshold and back to zero when idle without
  flapping; desired replicas never exceed the configured max; a stale/missing signal holds
  last-known-good rather than scaling to zero; `orpheus_autoscale_desired_replicas` reflects each
  decision.
- **Tests (real paths):** the streaming relay integration test stands up a fake worker WS and
  asserts bidirectional relay, byte-metering sets `audio_seconds` (`streaming_ws.go:196`), status
  transitions live→closing (`:219`), and the worker-outage 502 path (`:148`); the ListDeliveries
  edge table (`webhooks.go:419`) passes including invalid-status 400 and the LIMIT/org placeholder
  regression guard; the GPU soak runs clean for the target duration with no memory leak and bounded
  p99.

## 7. Dependencies, risks, open questions

- **Dependencies:** Modal metrics API access/token; a Prometheus + Grafana deployment; KEDA (or
  equivalent) if 4.5 uses scaled objects; the `model` dimension must be plumbed from processor
  params into both metrics and rollup.
- **Risks:** high-cardinality labels (per-model × per-status) can blow up Prometheus — bound the
  model label to a known set. Modal metrics may lag real-time, weakening tight autoscaling loops.
  Cost reconciliation depends on `gpu_seconds` being returned on every path (diarize has three
  return points — `:107/:123/:169` — all must include it).
- **Open questions:** scrape Modal metrics from the worker or a dedicated exporter? Is queue depth
  alone enough for scaling or do we need in-flight duration too? Where does the SLA status page
  live (dashboard vs public)? Retention/rollup granularity for GPU metrics vs the existing hourly
  cost rollup?

## 8. Effort

- 4.1 metrics collectors (API + worker): **S–M**.
- 4.6 relay + delivery tests: **S**; GPU soak harness: **M**.
- 4.2 Modal GPU exporter: **M**.
- 4.3 cost dashboards + `/v1/usage/cost`: **M**.
- 4.4 SLA/SLI + error budget: **M**.
- 4.5 autoscaling consumer: **M–L** (start with static thresholds).
- **Total:** ~1 quarter; the metrics+tests foundation (phase 1) is ~2 weeks and unblocks the rest.
