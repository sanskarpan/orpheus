# Design 09 — Temporal for multi-step workflows

> **Status:** proposed · **Owner:** platform / workers · **Scope:** gap #9
> **Depends on:** [ADR-0004 (Arq/Temporal)](../adr/0004-arq-temporal.md), [ADR-0005 (model versioning)](../adr/0005-model-versioning.md), [PRODUCTION_DESIGN §5.4, §6.1–6.3](../architecture/PRODUCTION_DESIGN.md)
> **Non-goal:** replacing Arq/NATS for single-activity `<60s` jobs. Those stay where they are.

This is a design + a **clearly-non-functional skeleton** under `apps/workflows/`.
Nothing here runs today. The build checklist at the end is the acceptance
criteria.

---

## 1. Problem

`transcribe-long` (and every future multi-step processor: `diarize+align`,
`demucs+remix`, `generate-and-mix`) is orchestrated **today** by a DB row plus a
single NATS-dispatched job:

- `POST /v1/workflows/transcribe-long` inserts a `workflows` row + one `jobs`
  row and emits a `job.queued` outbox event (see
  `apps/api/internal/handlers/workflows.go`). The workflow "state machine" is
  effectively `workflows.status` updated by whoever finished the job.
- The migration `0003_workflows.sql` gives us `workflows(status, current_job_id,
  result, error)` — a single-job tracker, **not** a multi-step orchestrator.

This is the pragmatic substitution called out in `docs/IMPLEMENTATION_STATUS.md`
("DB-tracked workflow instead of Temporal"). It is fine for one job. It breaks
the moment a workflow has **more than one step**, because the DB-tracked model
has no answer for:

| Requirement | DB-tracked today | Needed |
|---|---|---|
| Fan-out (chunk → parallel transcribe → stitch) | none | parallel child activities with backpressure |
| Partial failure of step N of M | job goes `failed`, workflow stuck | retry the step, not the whole workflow |
| Saga / compensation on cancel | none (orphaned S3 chunks, half-written results) | reverse-order compensation |
| Human-in-loop / signals (e.g. "approve before publish") | none | durable signal handlers |
| Exactly-once side effects across restarts | best-effort via outbox | deterministic replay + idempotent activities |
| Long timers (retry a GPU pool after 10 min) | cron hack | durable timers |
| Versioning a running workflow's code | redeploy = undefined behaviour | `workflow.get_version()` gates |

Temporal is the orchestrator ADR-0004 already chose for exactly this. This
design introduces it **for `transcribe-long` first**, behind a feature flag,
with a strangler-fig migration off the DB-tracked path.

---

## 2. Proposed architecture

```
                    Go API tier (unchanged public surface)
  POST /v1/workflows/transcribe-long
        │
        │  (feature flag: workflows.temporal.transcribe_long)
        ├── flag OFF ─► existing DB-tracked path (0003 handler)  ── legacy
        │
        └── flag ON  ─► TemporalGateway.StartWorkflow(...)  (gRPC → worker plane)
                              │
                              ▼
                   ┌──────────────────────────────────────────┐
                   │ Temporal service (Cloud in v1; self-host  │
                   │ later) — durable event history, timers,   │
                   │ signals, task queues                      │
                   └───────────────┬───────────────────────────┘
                                   │ polls task queue "transcribe-long"
                                   ▼
      apps/workflows/ (Python 3.12, separate deployment unit)
      ┌────────────────────────────────────────────────────────┐
      │ Workflow: TranscribeLongWorkflow  (deterministic)       │
      │   probe → decide(chunk?) → slice → fan-out transcribe   │
      │   → stitch → persist_result → emit_completed            │
      │   signal("cancel") → CancellationScope → compensations  │
      ├────────────────────────────────────────────────────────┤
      │ Activities (non-deterministic, do real I/O):            │
      │   probe_audio, slice_audio, transcribe_chunk (GPU),     │
      │   stitch_transcripts, persist_result, emit_outbox_event,│
      │   compensate_delete_chunks, compensate_mark_failed      │
      └───────────────┬─────────────────────┬──────────────────┘
                      │                     │
              Ray Serve (GPU)         Postgres / S3 (service role)
```

**Key placements**

- **Workflow code** (deterministic control flow) lives in
  `apps/workflows/orpheus_workflows/`. It calls **no** I/O directly — only
  activities.
- **Activities** (all real side effects: ffmpeg, GPU calls, DB writes, S3) are
  thin wrappers that **reuse the existing worker processors** in
  `apps/workers/src/orpheus_workers/processors/` (`probe`, `slice`,
  `transcribe`). We do not re-implement audio logic; the Temporal activity is an
  adapter.
- **The Go API tier does not embed a Temporal client for the hot path.** It
  calls a small gRPC method on the worker control plane
  (`orpheus_workers_control`) — `StartWorkflow` / `SignalWorkflow` /
  `DescribeWorkflow` — which owns the Temporal Python SDK client. This keeps the
  Go tier free of a Temporal dependency and matches the existing gRPC boundary
  (ADR-0013).

### 2.1 Workflow definition — `TranscribeLongWorkflow`

Pseudocode (the real thing is in the skeleton, marked non-functional):

```
@workflow.defn
class TranscribeLongWorkflow:
    @workflow.run
    async def run(self, input: TranscribeLongInput) -> TranscribeLongResult:
        # 1. probe (cheap, CPU) — decides whether to chunk
        probe = await execute_activity(probe_audio, input.artifact_key,
                                       start_to_close=timeout(60s),
                                       retry=policy(max=5))
        chunks = plan_chunks(probe.duration_s, target=CHUNK_SECONDS)  # pure fn

        # 2. slice into S3 chunk keys (compensatable side effect)
        chunk_keys = await execute_activity(slice_audio, input.artifact_key, chunks,
                                            start_to_close=timeout(10m))
        self._compensations.append(("delete_chunks", chunk_keys))

        # 3. fan-out transcribe (bounded parallelism via asyncio.Semaphore)
        results = await gather_bounded(
            [execute_activity(transcribe_chunk, k, input.params,
                              task_queue="gpu-transcribe",
                              start_to_close=timeout(15m),
                              heartbeat=timeout(60s),
                              retry=policy(max=3))
             for k in chunk_keys],
            limit=input.max_parallel_chunks)

        # 4. stitch (deterministic offsets applied in an activity)
        stitched = await execute_activity(stitch_transcripts, results, chunks)

        # 5. persist under service role + emit outbox event (idempotent)
        await execute_activity(persist_result, input.workflow_id, stitched,
                               input.model_version_id)
        await execute_activity(emit_outbox_event, "workflow.completed",
                               input.workflow_id)
        return stitched
```

- **Determinism rule:** the workflow may use only `workflow.now()`,
  `workflow.random()`, `workflow.get_version()` and activity calls. No wall
  clock, no `os`, no direct network. Chunk planning (`plan_chunks`) is a pure
  function so it replays identically.
- **Bounded fan-out** protects the GPU pool (per-tenant bulkhead, §12 of the
  main design). Parallelism is an input, defaulted from the tenant plan.
- **`gpu-transcribe` task queue** is served by GPU worker pods; CPU activities
  run on the CPU pool queue. Temporal task queues == the pool boundary.

### 2.2 Saga / compensation on cancel

Cancellation is first-class (matches `PRODUCTION_DESIGN §6.2`):

1. `POST /v1/workflows/{id}/cancel` → API CAS-marks intent → gRPC
   `SignalWorkflow(id, "cancel", reason)`.
2. The workflow runs under a `CancellationScope`. On signal (or Temporal
   server-side cancel), in-flight activities receive cancellation via heartbeat;
   `transcribe_chunk` checks `activity.is_cancelled()` between segments.
3. Compensations run **in reverse order** of the recorded stack:
   `compensate_delete_chunks(chunk_keys)` (remove orphaned S3 slices) →
   `compensate_mark_failed(workflow_id, "cancelled")` (write terminal state +
   `workflow.cancelled` outbox event).
4. Compensations are themselves activities with their own retry policy, so a
   transient S3 error during cleanup does not leak chunks.

**Compensation is idempotent**: deleting an already-deleted chunk key is a
no-op; marking an already-terminal workflow is a CAS that returns "already
done". This is required because Temporal may re-run a compensation on worker
crash.

### 2.3 Idempotency

Three layers, because Temporal guarantees *at-least-once* activity execution:

| Layer | Mechanism |
|---|---|
| **Workflow start** | Workflow ID = `wf-transcribe-long-{workflow_row_id}`. Temporal rejects a duplicate start with the same ID (`WorkflowExecutionAlreadyStarted`), so the API's own idempotency-key middleware + this ID make double-submit safe. |
| **Activity side effects** | Every activity is written to be idempotent. `slice_audio` writes to deterministic S3 keys (`chunks/{workflow_id}/{index}.wav`) so a retry overwrites, not duplicates. `persist_result` is an `UPDATE ... WHERE status <> 'completed'` CAS. `emit_outbox_event` uses the existing outbox table's dedup (aggregate_id + event_type). |
| **GPU calls** | `transcribe_chunk` is pinned to a `model_version_id`; same input + same model = same output (reproducibility contract, ADR-0005). Safe to retry. |

### 2.4 Worker deployment

- **New deployment unit:** `apps/workflows/` builds a Python image that runs
  `python -m orpheus_workflows.worker`, registering the workflow(s) and
  activities against the `transcribe-long` and `gpu-transcribe` task queues.
- Reuses the existing worker base image / `uv` workspace so processors are
  importable. Runs in the **gVisor** sandbox like the other workers (ADR-0008),
  no network egress except Temporal + S3 + Postgres + Ray Serve VPC endpoints.
- **Scaling:** KEDA on Temporal task-queue backlog (`ScheduleToStart` latency)
  for the CPU-activity workers; GPU workers scale on the same signal plus GPU
  utilization, same as the existing GPU pool.
- **v1 uses Temporal Cloud** (ADR-0004 explicitly defers self-hosting until ops
  capacity exists). The only infra change for v1 is a Temporal Cloud namespace +
  mTLS client cert in Secrets Manager.

---

## 3. Data-model changes

We do **not** modify existing migrations. New migration `0005_workflow_temporal.sql`
(author it under `apps/api/internal/db/migrations/` — **out of scope for this
skeleton**, listed in the checklist) adds, additively:

```sql
ALTER TABLE workflows
  ADD COLUMN temporal_workflow_id text UNIQUE,     -- null until migrated
  ADD COLUMN temporal_run_id      text,            -- current run
  ADD COLUMN engine text NOT NULL DEFAULT 'db'      -- 'db' | 'temporal'
    CHECK (engine IN ('db','temporal'));

CREATE TABLE workflow_steps (                        -- observability mirror
  id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  workflow_id   uuid NOT NULL REFERENCES workflows(id) ON DELETE CASCADE,
  org_id        uuid NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
  name          text NOT NULL,        -- 'probe','slice','transcribe_chunk',...
  index         int  NOT NULL,
  status        text NOT NULL DEFAULT 'pending',
  attempt       int  NOT NULL DEFAULT 0,
  started_at    timestamptz,
  finished_at   timestamptz,
  error         text
);
-- RLS: identical policy shape to workflows (tenant_select/insert/update/delete)
```

`workflow_steps` is a **read model** projected from Temporal history by
`emit_outbox_event`-style activities. Temporal is the source of truth for
control flow; Postgres holds the queryable projection so the admin UI (design
#11) and `GET /v1/workflows/{id}` don't have to call Temporal for every read.

---

## 4. API surface

Public surface is **unchanged** (backward compatible). Additions:

```
POST   /v1/workflows/transcribe-long     # existing; now routes on the flag
GET    /v1/workflows/{id}                 # existing; reads workflows + steps
POST   /v1/workflows/{id}/cancel          # NEW: signals Temporal (or CAS legacy)
GET    /v1/workflows/{id}/steps           # NEW: step projection (admin/debug)
```

Internal gRPC (worker control plane, `packages/proto`):

```
service WorkflowControl {
  rpc StartTranscribeLong (StartReq)  returns (StartResp);   // wf_id, run_id
  rpc SignalWorkflow      (SignalReq) returns (Empty);       // cancel, approve
  rpc DescribeWorkflow    (DescribeReq) returns (WorkflowStatus);
}
```

---

## 5. Rollout plan (strangler fig)

1. **Ship dark.** Deploy `apps/workflows/` + Temporal Cloud namespace. Flag
   `workflows.temporal.transcribe_long` defaults **off**. No traffic.
2. **Shadow.** For a % of requests, run the Temporal workflow in parallel with
   the DB-tracked path, discard the Temporal result, compare (transcript diff,
   latency, cost). Alert on divergence.
3. **Canary.** Flag on for internal org, then 1% → 10% → 50% of tenants. The
   legacy path stays as instant fallback (flag off) for any tenant.
4. **Cut over.** Flag on globally. New workflows have `engine='temporal'`.
5. **Drain + retire.** Legacy DB-tracked workflows finish naturally
   (`engine='db'`). Once none remain in-flight, delete the legacy branch in the
   Go handler and mark the DB-tracked orchestration removed in
   IMPLEMENTATION_STATUS.
6. **Extend.** Repeat the pattern for `diarize+align` (design #10),
   `demucs+remix`, `generate-and-mix`.

Rollback at any step = flip the flag off; in-flight Temporal workflows continue,
new ones take the legacy path.

---

## 6. Risks

| Risk | Likelihood | Mitigation |
|---|---|---|
| Non-determinism bug (someone calls `time.time()` in workflow code) | Medium | `pytest` replay tests on recorded histories; `workflow.get_version()` discipline; lint rule banning stdlib time/random/os in `orpheus_workflows/*.py` (not activities). |
| Temporal Cloud cost / lock-in | Low–Med | Cloud in v1 only; self-host path is documented in ADR-0004; workflow code is portable (SDK, not Cloud-specific). |
| Duplicate side effects on activity retry | Medium | Idempotent activities (§2.3); deterministic S3 keys; CAS DB writes. |
| Orphaned S3 chunks on crash mid-cancel | Low | Compensations are activities with their own retries; a nightly reaper GC's `chunks/{workflow_id}/` for terminal workflows as a backstop. |
| Two orchestration models coexisting (DB + Temporal) confusing ops | Medium | `engine` column is explicit; dashboards split by engine; strangler retires the legacy path fast. |
| GPU pool starvation from unbounded fan-out | Medium | Bounded parallelism input + per-tenant bulkhead + task-queue-level worker caps. |
| Workflow code deploy breaks running executions | Medium | `workflow.get_version()` gates all control-flow changes; blue/green worker deploy; never remove an old version branch until no history references it. |

---

## 7. What must be built (checklist)

- [ ] `apps/workflows/` deployment unit: `pyproject.toml` in the uv workspace,
      Dockerfile (reuse worker base + gVisor), `entrypoint` running
      `python -m orpheus_workflows.worker`.
- [ ] `temporalio` SDK dependency + Temporal Cloud namespace + mTLS cert wiring
      via External Secrets.
- [ ] `TranscribeLongWorkflow` (deterministic) — replace the skeleton's `raise
      NotImplementedError`s.
- [ ] Activities as adapters over existing processors: `probe_audio`,
      `slice_audio`, `transcribe_chunk`, `stitch_transcripts`, `persist_result`,
      `emit_outbox_event`, `compensate_delete_chunks`, `compensate_mark_failed`.
- [ ] `plan_chunks` pure function + unit tests + a **workflow replay test**
      against a recorded history (determinism guard).
- [ ] Bounded fan-out helper (`gather_bounded`) honoring `max_parallel_chunks`.
- [ ] Cancellation: `CancellationScope`, signal handler, reverse-order
      compensation stack, heartbeating in `transcribe_chunk`.
- [ ] gRPC `WorkflowControl` service in `packages/proto` + Go client in the API
      tier + Python server in `orpheus_workers_control`.
- [ ] Migration `0005_workflow_temporal.sql` (`engine`, `temporal_workflow_id`,
      `temporal_run_id`, `workflow_steps` table + RLS). **Do not edit 0003.**
- [ ] Go handler: route `transcribe-long` on the flag; add `/cancel` and
      `/steps`; keep the legacy branch until drained.
- [ ] Feature flag `workflows.temporal.transcribe_long` (Flipt).
- [ ] KEDA scaler on Temporal task-queue backlog; gVisor runtimeClass on the
      workflow pods.
- [ ] Shadow-compare harness + divergence alert (rollout step 2).
- [ ] Runbook: how to cancel/terminate a stuck workflow, replay a poisoned one,
      and interpret `workflow_steps`.
