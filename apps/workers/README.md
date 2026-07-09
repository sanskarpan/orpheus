# `orpheus-workers`

Python worker plane for Orpheus. Executes async audio processing jobs
dispatched by the Go API tier.

## Phase 0

Empty. This package exists so the workspace structure is settled and
`uv sync --all-packages` picks it up. No code, no dependencies.

## Phase 2 plan

### Components

| Component       | Tech                              | Purpose |
|-----------------|-----------------------------------|---------|
| Control plane   | FastAPI                           | Worker registration, health, `/metrics` |
| Short-job queue | [arq](https://arq-docs.helpmanual.io) on Redis | Sub-minute jobs (e.g. classify, brief transcribe) |
| Workflows       | [temporalio](https://temporal.io) | Long-running pipelines (e.g. transcribe-long) |
| GPU inference   | [Ray Serve](https://docs.ray.io)  | ASR, separation, generation |
| Observability   | structlog, opentelemetry-\*       | Logs, traces, metrics |

### Layout (planned)

```
apps/workers/
├── pyproject.toml
├── README.md
├── src/orpheus_workers/
│   ├── __init__.py
│   ├── control/                  # FastAPI control plane
│   ├── queues/                   # arq workers
│   ├── workflows/                # Temporal activities & workflows
│   ├── inference/                # Ray Serve deployments
│   ├── jobs/                     # Job model + state machine
│   ├── telemetry/                # OTel/logging setup
│   └── gen/                      # Generated Protobuf stubs (Phase 1+)
└── tests/
```

### Coordination with the Go API

Workers receive jobs over two channels:

1. **gRPC** (`packages/proto/`, Phase 1+) — primary channel for
   streaming long jobs and progress updates.
2. **Redis (arq)** — short jobs that don't need a durable workflow.

The Go API enqueues work; workers consume and report back via gRPC
(`JobService.UpdateStatus`) and Postgres (job ledger).

### References

- `docs/architecture/PRODUCTION_DESIGN.md` §6 (Service topology) and
  §7 (Worker plane).
