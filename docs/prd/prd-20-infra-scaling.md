# PRD: Infrastructure & Scaling — GPU Batching, Autoscaling, Dynamic Concurrency & Model Routing

**Status:** Proposed · **Priority:** P1 · **Epic:** Platform Scaling · **Related issues:** #325, #420, #326, #421, #423

## 1. Summary

Orpheus's job pipeline is correct but under-optimized for GPU economics and load.
Each transcription runs one audio file per model call — the Modal endpoint decodes
individually (`infra/modal/orpheus_transcribe.py:111`), the worker's concurrency is
statically configured (`config.py:16`, `:19`), autoscaling is left entirely to
Modal's request-driven scaler, and the runtime loads whichever whisper model the
worker was configured with rather than the checksum-verified artifact in the model
registry (`model_registry.py:95`). The `batching` package aggregates *results* for
delivery (`internal/batching/service.go:1`) — it does **not** batch GPU inference.

This PRD adds the missing scaling machinery: (1) **dynamic/continuous GPU batching**
of inference requests, (2) **in-app autoscaling** driven by the JetStream
queue-depth gauge we already publish, (3) **dynamic concurrency** replacing the
static worker settings, (4) wiring the **model registry** into runtime loading, and
(5) **multi-model routing / per-tier engine selection**. All GPU work stays on the
Modal service pattern (`@app.cls`, `@modal.concurrent`, `min_containers`/
`max_containers`).

## 2. Motivation & goals

**Goals**
- Increase GPU throughput and cut cost/minute via request batching on the Modal GPU.
- Scale worker capacity to the *actual* backlog using `orpheus_jetstream_pending_messages`
  (`worker.py:52`, `metrics.py:11`) instead of a fixed worker count.
- Make per-worker and per-org concurrency adaptive rather than static
  (`config.py:16`, `:19`).
- Load models by verified `(name, version, sha256)` from the registry
  (`model_registry.py:95`) so runtime weights are reproducible and tamper-checked.
- Route jobs to the right engine/model per tier (fast vs. accurate; CPU vs. GPU).

**Non-goals**
- Replacing Modal with self-hosted GPU orchestration (Modal stays the GPU substrate).
- Training/fine-tuning models (registry is inference-only).
- Rewriting the NATS job bus (JetStream stays; we only read its depth).

## 3. Current state in Orpheus (file:line, patterns to build on)

- **Worker consumer**: `worker.py:80` (`start`) subscribes to `ORPHEUS_JOBS`
  (`worker.py:42`), one message → one job (`_handle_job_queued`, `worker.py:173`).
  Per-org concurrency cap deferred via `nak(delay=5)` at `worker.py:202`, checked
  against `per_org_concurrency` (`config.py:19`). Global `worker_concurrency`
  (`config.py:16`) is defined but effectively bounds only intended parallelism.
- **Queue-depth gauge already exists**: `record_queue_depth` polls the JetStream
  consumer's `num_pending` and sets `orpheus_jetstream_pending_messages`
  (`worker.py:52`, gauge at `metrics.py:11`); polled every
  `queue_depth_poll_seconds` (`config.py:29`, default 15s) via `_poll_queue_depth`
  (`worker.py:123`). **Nothing consumes this gauge to scale.**
- **Modal GPU services** follow one pattern: `@app.cls(gpu="a10g", min_containers=0,
  scaledown_window=300, timeout=1800)` + `@modal.concurrent(max_inputs=4,
  target_inputs=2)` (`infra/modal/orpheus_transcribe.py:45`, `:54`); diarize uses
  `@modal.concurrent(max_inputs=4)` (`orpheus_diarize.py:56`), llm `max_inputs=8`
  (`orpheus_llm.py:51`). Transcription decodes one file per call
  (`orpheus_transcribe.py:111`), holding a per-model `WhisperModel` cache
  (`orpheus_transcribe.py:60`).
- **Backend switch**: `transcribe.py:34` `_backend()` returns `local`|`modal` from
  `ORPHEUS_WORKER_TRANSCRIBE_BACKEND`; modal path at `transcribe.py:181`.
- **Model registry (unused at runtime)**: `model_registry.py:95` `resolve(name,
  version)` downloads from S3, verifies sha256 (`ModelChecksumError`,
  `model_registry.py:119`), caches locally. The transcribe path instead constructs
  `WhisperModel(model_size, ...)` directly (`orpheus_transcribe.py:69`,
  `transcribe.py`), bypassing the registry.
- **Result batching (not inference)**: `internal/batching/service.go:133`
  aggregates child-job counts and pushes results — explicitly *not* GPU batching.
- **Cost metering** already reads `gpu_seconds` per result (`worker.py:250`), so any
  batching change must keep attributing GPU seconds per job.

## 4. Proposed design

### 4.1 Dynamic / continuous GPU batching (#325, #420)

Batch multiple audio inputs into one GPU model invocation on the Modal transcribe
service, using faster-whisper's `BatchedInferencePipeline` (batched decode) — the
`@modal.concurrent` fan-in is the natural batching window.

Design on the existing `Transcriber` class (`orpheus_transcribe.py:55`):
- Raise `@modal.concurrent(max_inputs=N, target_inputs=B)` so Modal delivers up to
  `B` concurrent `transcribe` calls into one container (today `max_inputs=4,
  target_inputs=2`, `:54`).
- Wrap the model in `BatchedInferencePipeline(model)` and add a micro-batch collector
  (`@modal.batched(max_batch_size=B, wait_ms=…)` where available, else an in-process
  async collector) that groups pending decodes, runs one batched `pipeline.transcribe`,
  and demuxes segments back to callers.
- Preserve the return contract per input: `{text, segments, language,
  duration_seconds, gpu_seconds}` (`orpheus_transcribe.py:136`). **Attribute
  `gpu_seconds` per input** by splitting the batch wall-time proportionally to each
  input's audio duration, so metering (`worker.py:250`) stays fair and per-job.

This is opt-in behind `ORPHEUS_TRANSCRIBE_BATCH_SIZE`; size 1 == today's behavior.
vLLM-style continuous batching is the same idea for the LLM service
(`orpheus_llm.py`) and lands as part of M3 once the transcribe path proves the
demux/metering approach.

**Failure modes & graceful degradation.** A single bad input in a batch must not
fail the whole batch: run per-input decode inside a guarded task, and on a
per-input exception demux an error result for that job only while the rest of the
batch completes — the healthy jobs return normally and the poisoned job fails with
its own `ModelDecodeError`. If the batched pipeline itself raises (OOM, CUDA
fault), fall back to serial single-input decode for that window (size-1 path,
`orpheus_transcribe.py:111`) so throughput degrades but correctness holds; emit
`orpheus_transcribe_batch_fallback_total` so the degradation is alertable.

**Scale, concurrency & bounded cost.** `max_inputs`/`target_inputs` on
`@modal.concurrent` (`:54`) bound the fan-in window; `max_batch_size` bounds GPU
memory. The collector holds a hard cap on `wait_ms` so a partial batch flushes
rather than starving latency-sensitive jobs (backpressure toward latency, not
unbounded queueing). Per-container GPU memory is sized so
`max_batch_size × largest routed model` fits with headroom; `max_containers`
(from M4 autoscaler) caps standing GPU cost.

**Observability & metering.** Every batched invocation records batch size,
per-input `gpu_seconds`, wall-time, and fallback count. The proportional
`gpu_seconds` split (audio-duration-weighted) keeps per-job cost metering
(`worker.py:250`) fair; a batch-vs-serial metering-parity check runs in CI (§6).

### 4.2 In-app autoscaling from JetStream queue depth (#326, #421)

We already publish backlog (`orpheus_jetstream_pending_messages`, `metrics.py:11`).
Add a small **autoscaler control loop** (new `control_plane` responsibility;
`control_plane.py` already exists) that:
- Reads `num_pending` (reuse `record_queue_depth`, `worker.py:52`) plus running-job
  count, computes desired capacity from a target backlog-per-worker ratio, and
- **Scales the Modal GPU** by setting `min_containers`/`max_containers` on the
  transcribe app via Modal's API (today `min_containers=0`, `orpheus_transcribe.py:50`)
  — e.g. raise `min_containers` when `pending > high_watermark` to pre-warm GPUs, drop
  to 0 when idle (keeping `scaledown_window=300`, `:51`).
- For CPU workers, expose desired replica count as a gauge that a k8s HPA / Modal
  scaler consumes.

Guardrails: min/max bounds, cooldown between scale actions, and a hysteresis band so
a flapping queue doesn't thrash containers. Purely additive — no change to the job
consume path.

**Failure modes & bounded cost.** The autoscaler is fail-safe: if the Modal control
API is unreachable or returns an error, it holds the last-known-good
`min/max_containers` rather than scaling blindly, and never exceeds a
hard-configured `ORPHEUS_AUTOSCALE_MAX_CONTAINERS` ceiling regardless of backlog —
a runaway backlog cannot burn unbounded GPU. A watchdog forces `min_containers`
back to its floor if the loop stops heartbeating (a crashed autoscaler must not
leave GPUs pinned warm). Scale-up is rate-limited (one step per cooldown) so a
transient spike doesn't provision the whole ceiling at once.

**Multi-tenant fairness.** Capacity is a shared pool; the autoscaler scales on
aggregate backlog while per-org concurrency (§4.3) prevents a single tenant's
backlog from monopolizing the pool it triggered.

**Observability.** Emit `orpheus_autoscale_desired_containers`,
`orpheus_autoscale_action_total{direction}`, and `orpheus_autoscale_api_errors_total`
so every scale decision, its input backlog, and any control-API failure are visible
and alertable.

### 4.3 Dynamic concurrency (replaces static config)

`worker_concurrency` (`config.py:16`) and `per_org_concurrency` (`config.py:19`) are
constants. Make them adaptive:
- **Per-worker**: derive an effective in-flight limit from measured job latency +
  host resource pressure; a worker under memory pressure lowers its own limit rather
  than accepting work it will thrash on.
- **Per-org**: keep the fairness deferral at `worker.py:202` but source the cap from a
  per-org tier (DB-backed `org_limits.concurrency`) instead of the single global
  `per_org_concurrency` — a paid tier gets a higher cap. The `running_jobs_for_org`
  check (`worker.py:202`) reads the tiered value; default falls back to
  `config.py:19`.
- Values are re-read on the existing control hot-reload channel (`worker.py:114`
  `_on_control`, subject `worker.py:47`) so limits change without a redeploy.

**Failure modes.** If the per-org tier lookup (DB-backed `org_limits.concurrency`)
fails or the row is missing, fall back to the static `per_org_concurrency`
(`config.py:19`) — a DB blip must never remove the fairness cap or block work.
An out-of-range or malformed control-channel value is rejected and the prior value
retained (no unbounded in-flight from a fat-fingered reload). The per-worker
adaptive limit has a hard floor of 1 (a pressured worker still makes forward
progress) and a hard ceiling of `worker_concurrency` (`config.py:16`).

**Multi-tenant security & isolation.** The tiered cap is read under the tenant RLS
context; one org can never read or raise another org's limit. `running_jobs_for_org`
(`worker.py:202`) counts only the owning org's in-flight jobs, preserving isolation.

**Observability.** Emit `orpheus_worker_effective_concurrency` and
`orpheus_org_concurrency_deferrals_total{org}` (bounded-cardinality) so throttling
and fairness deferrals are visible per tier.

### 4.4 Wire the model registry into runtime loading (#423)

Today transcribe builds `WhisperModel(model_size, download_root=CACHE_DIR)` directly
(`orpheus_transcribe.py:69`) — no sha256 verification, weights come from HF cache.
Route model acquisition through `ModelRegistry.resolve` (`model_registry.py:95`):
- Before constructing `WhisperModel`, call `resolve(name, version)` to get a
  **verified local path** (raises `ModelChecksumError`, `:119`, on tamper/corruption),
  then load `WhisperModel(str(path), ...)`.
- The `(processor, version) → model_version_id` binding from the catalog
  (resolved in `jobs.go:158`) selects which registry row to load, closing the loop
  between the API's reproducibility promise and the bytes actually executed.
- On Modal, `resolve` targets the mounted Volume cache (`orpheus_transcribe.py:41`)
  so verified weights persist across cold starts; the sha256 check runs on cache hits
  too (`model_registry.py:113`).
- Registration stays as-is (`model_registry.py:48`, S3 + sha256 recorded).

**Failure modes & integrity.** A `ModelChecksumError` (`model_registry.py:119`) is
terminal for the job — it fails cleanly rather than transcribing with unverified
weights (integrity over availability). A transient S3 fetch error is retried with
backoff before failing. During migration, an env-gated fallback
(`ORPHEUS_MODEL_REGISTRY_STRICT=false`) may permit HF-cache load for models not yet
registered, logging `orpheus_model_registry_fallback_total`; strict mode (prod
default once backfill completes) makes an unregistered model a hard error.

**Scale & cold starts.** The verified-path load is cached on the Modal Volume so the
sha256 check, not a re-download, is the steady-state cost; concurrent cold starts on
the same model are single-flighted so N containers don't redundantly download the
same blob.

**Observability.** Record the resolved `(name, version, sha256)` on every job result
so the exact executed bytes are auditable and reproducible; emit
`orpheus_model_resolve_seconds` and `orpheus_model_checksum_failures_total`.

### 4.5 Multi-model routing / per-tier engine selection

Introduce an engine-selection layer in the transcribe processor (`transcribe.py`)
keyed off job params + org tier:
- **Model routing**: `params.model` already flows to Modal (`orpheus_transcribe.py:86`);
  formalize a catalog of `(quality_tier → model)` — e.g. `fast` → `distil-large-v3`,
  `accurate` → `large-v3`, `turbo` → `large-v3-turbo` (default, `:30`).
- **Engine routing**: choose CPU vs. Modal GPU per job by size/tier rather than a
  single global `_backend()` env (`transcribe.py:34`): short clips or free-tier →
  local CPU; long/paid → Modal GPU. Selection resolves to the existing backends so no
  new execution path is needed.
- **Registry-backed**: each routed model is a registry `(name, version)` (§4.4), so
  routing selects verified weights and the choice is recorded in the result for
  reproducibility + metering.

**Failure modes & fallback.** If a routed model is unavailable (registry miss,
GPU-tier capacity exhausted), routing falls back down the tier ladder (e.g.
`accurate` GPU busy → `turbo` GPU, or GPU unavailable → CPU `local` for short
clips) rather than failing the job, recording the actual engine/model used so
metering and reproducibility reflect reality. A job that pins an explicit
`params.model` is never silently downgraded — an unavailable pinned model is a hard
error, not a substitution.

**Scale, cost & security.** Engine selection is the primary cost lever: free-tier
and short clips stay on CPU (no GPU spend), long/paid jobs use GPU. The routing
table and per-tier caps are resolved under the tenant RLS context so tier is
authoritative and not client-spoofable. Backward-compatible: `params.model` still
flows to Modal exactly as today (`orpheus_transcribe.py:86`); routing only fills the
default when a tier (not an explicit model) is requested, so existing on-wire
requests are unchanged.

**Observability.** Emit `orpheus_route_selected_total{tier, engine, model}` and
`orpheus_route_fallback_total{from, to}` to see routing distribution and how often
degradation fires.

## 5. Rollout / milestones

Every milestone is itself production-quality and shippable — full error handling,
metering, and observability from the first ship. Milestones are ordering only, not
reduced-scope prototypes; the complete feature set in §4 is in scope.

1. **M1 — Model registry at runtime (#423).** Lowest risk, highest integrity win:
   route every `WhisperModel` load through `resolve` with sha256 verification,
   terminal `ModelChecksumError`, transient-retry, single-flight cold-start, resolved
   `(name, version, sha256)` recorded on results, and the strict/fallback env gate for
   migration. Ships with `orpheus_model_checksum_failures_total` and the model backfill.
2. **M2 — Dynamic concurrency.** Per-org tiered cap (DB-backed, RLS-scoped) + control-
   channel reload with value validation; per-worker adaptive limit with hard floor/
   ceiling; DB-miss fallback to static config; fairness/deferral metrics. Production-
   grade fairness with no static-config regression.
3. **M3 — GPU batching (#325, #420)** behind `ORPHEUS_TRANSCRIBE_BATCH_SIZE`, with
   per-input `gpu_seconds` attribution, per-input error isolation, batched→serial
   fallback on batch failure, and a CI metering-parity gate before default-on. Includes
   the vLLM-style continuous-batching path for `orpheus_llm.py`.
4. **M4 — Autoscaler loop (#326, #421)** consuming the queue-depth gauge, driving Modal
   `min/max_containers` with hysteresis, cooldown, a hard `MAX_CONTAINERS` ceiling,
   fail-safe hold on control-API error, and a watchdog that returns to floor if the loop
   stalls. Ships with full scale-decision metrics.
5. **M5 — Multi-model / engine routing** (tier catalog + CPU/GPU selection) with
   tier-ladder fallback, no-silent-downgrade for pinned models, RLS-authoritative tier
   resolution, and routing-distribution metrics.

## 6. Verification / acceptance criteria

End-to-end against a real worker + the deployed Modal apps (`orpheus-transcribe`,
`orpheus-llm`), not unit-only. Each criterion asserts a happy path, a negative/failure
path, and where relevant multi-tenant isolation.

- **Registry (happy + failure)**: a job pinned to a `(model name, version)` loads the
  exact registry blob and the result records the resolved `(name, version, sha256)`.
  A deliberately corrupted Volume/S3 object raises `ModelChecksumError`
  (`model_registry.py:119`), the job fails cleanly (no transcription with bad weights),
  and `orpheus_model_checksum_failures_total` increments. A transient S3 error retries
  then succeeds. In strict mode an unregistered model is a hard error; in migration
  mode it falls back to HF-cache and increments the fallback counter.
- **Batching (throughput + isolation + fallback)**: at `BATCH_SIZE=B` on the live
  Modal endpoint, GPU throughput (minutes-of-audio/GPU-second) rises measurably vs.
  `BATCH_SIZE=1` on a fixed workload; each job returns correct `{text, segments,
  language}` and a per-input `gpu_seconds` whose sum ≈ batch wall-time within the
  documented tolerance (metering parity, `worker.py:250`). A poisoned input in a batch
  fails only its own job while the rest complete. Forcing a batch-level fault triggers
  serial fallback and `orpheus_transcribe_batch_fallback_total` increments; results
  stay correct.
- **Autoscaler (scale + bound + fail-safe)**: injecting a backlog raises
  `orpheus_jetstream_pending_messages` (`metrics.py:11`) and the loop raises Modal
  `min_containers` without ever exceeding `ORPHEUS_AUTOSCALE_MAX_CONTAINERS`; draining
  scales back to 0 after `scaledown_window` with no flapping under a steady trickle
  (hysteresis holds). Simulating a Modal control-API error holds last-known-good and
  increments `orpheus_autoscale_api_errors_total`; killing the loop lets the watchdog
  return `min_containers` to floor.
- **Dynamic concurrency (fairness + isolation + fallback)**: a paid-tier org runs more
  concurrent jobs than the default cap while a free-tier org is still deferred at
  `worker.py:202`; org A cannot observe or affect org B's cap (RLS-scoped). A tier-
  lookup DB failure falls back to `config.py:19` with no lost cap. A malformed control-
  channel value is rejected and the prior limit retained; limits otherwise change via
  the control channel (`worker.py:114`) without restart.
- **Routing (selection + no-silent-downgrade)**: `quality_tier=fast` vs `accurate`
  selects different registry models, recorded in the job result; short free-tier clips
  run on CPU, long paid clips on Modal GPU. A busy GPU tier degrades down the ladder and
  records the actual engine used; an explicitly pinned unavailable `params.model` fails
  hard rather than substituting. Tier is resolved server-side and cannot be spoofed by
  the client.
- **Backward compatibility**: existing requests that pass `params.model` and no tier
  behave byte-identically to today's on-wire contract.

## 7. Dependencies, risks, open questions

- **Batching correctness**: per-input segment demux must not cross-contaminate
  outputs; word-timestamp mode (`orpheus_transcribe.py:90`) may constrain batch size.
- **Metering fairness under batching**: proportional `gpu_seconds` split is an
  approximation — validate it doesn't systematically over/under-charge vs. serial.
- **Autoscaler authority**: driving Modal `min/max_containers` needs Modal API
  credentials in the control plane and careful cost guardrails (a runaway
  `min_containers` burns standing GPU cost, the very thing `min_containers=0` avoids).
- **Registry coverage**: whisper models must be registered before routing can load
  them; needs a one-time backfill/`register` (`model_registry.py:48`) of current
  models and a fallback to HF-cache load during migration.
- **Cold starts**: batching raises per-container memory (multiple/bigger models in the
  cache, `orpheus_transcribe.py:60`); size GPU + `max_inputs` accordingly.
- **Open**: batch window (`wait_ms`) latency vs. throughput tradeoff per tier? Should
  the autoscaler live in the Go control plane or the Python `control_plane.py`? Do we
  keep a global backend env as an override once per-job engine routing lands?

## 8. Effort

- M1 (registry wiring): ~1.5 weeks + model backfill (adds retry, single-flight,
  strict/fallback gate, result provenance).
- M2 (dynamic concurrency): ~2 weeks (config + tiered caps + RLS + reload validation
  + fallback + metrics).
- M3 (GPU batching): ~3 weeks (batched pipeline + demux + per-input isolation +
  serial fallback + metering-parity gate).
- M4 (autoscaler): ~2.5 weeks (control loop + Modal API + hysteresis + hard ceiling +
  fail-safe + watchdog + metrics).
- M5 (multi-model/engine routing): ~2 weeks (tier catalog + CPU/GPU selection +
  ladder fallback + no-silent-downgrade).
- **Total: ~1 quarter** for 1–2 platform engineers; M1–M2 are low-risk and unlock
  reproducibility + fairness before the throughput work lands. Each milestone ships
  production-grade (full error handling, metering, observability), not as a prototype.
