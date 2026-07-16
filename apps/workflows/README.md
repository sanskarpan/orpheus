# `apps/workflows` — Temporal workflow plane (SCAFFOLD, non-functional)

> **Status: skeleton only.** Nothing in this directory runs. It is the landing
> zone for gap #9 (Temporal multi-step workflows). See the design doc:
> [`docs/design/09-temporal-workflows.md`](../../docs/design/09-temporal-workflows.md).

## Why this exists

`transcribe-long` and future multi-step processors (`diarize+align`,
`demucs+remix`, `generate-and-mix`) are orchestrated today by a **DB-tracked
workflow** (`workflows` table + one NATS-dispatched job — see
`apps/api/internal/handlers/workflows.go` and migration `0003_workflows.sql`).
That model has no fan-out, no saga/compensation, no signals, and no durable
timers. ADR-0004 chose **Temporal** for exactly this. This deployment unit will
host the Temporal **workflows** (deterministic control flow) and **activities**
(side-effecting adapters over the existing processors in
`apps/workers/src/orpheus_workers/processors/`).

## What lives here (target shape)

```
apps/workflows/
  README.md                      ← you are here
  pyproject.toml                 ← (TODO) uv-workspace member, temporalio dep
  Dockerfile                     ← (TODO) reuse worker base + gVisor runtimeClass
  orpheus_workflows/
    __init__.py                  ← (TODO)
    worker.py                    ← (TODO) registers workflows+activities, polls task queues
    activities.py                ← (TODO) adapters over probe/slice/transcribe processors
    transcribe_long/
      __init__.py                ← (TODO)
      workflow.py                ← SKELETON (this commit) — deterministic control flow
      planning.py                ← (TODO) pure plan_chunks() + unit tests
    _replay_tests/               ← (TODO) recorded histories + determinism guards
```

Only `orpheus_workflows/transcribe_long/workflow.py` exists so far, and it is a
**clearly-marked non-functional skeleton** (every step raises
`NotImplementedError`). It is here to pin the interface and the determinism
contract, not to run.

## Boundaries (do not violate)

- **Workflow code is deterministic.** No wall clock, no `os`, no direct network,
  no randomness except `workflow.now()` / `workflow.random()`. All I/O goes
  through **activities**.
- **Activities reuse existing processors.** Do not re-implement ffmpeg /
  whisper logic here; import from `orpheus_workers.processors`.
- **The Go API tier does not import Temporal.** It calls the worker control
  plane over gRPC (`WorkflowControl`), which owns the Temporal client.

## Running (once built — not yet)

```bash
# NOT FUNCTIONAL YET — placeholder command shape
uv run python -m orpheus_workflows.worker   # polls 'transcribe-long' + 'gpu-transcribe'
```

## Build checklist

See the "What must be built" section of
[`docs/design/09-temporal-workflows.md`](../../docs/design/09-temporal-workflows.md).
