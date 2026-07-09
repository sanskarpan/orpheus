# `orpheus-proto`

gRPC / Protobuf contracts between the **Go API** (`apps/api/`) and the
**Python workers** (`apps/workers/`). The single source of truth for
cross-service types and RPCs.

## Phase 0

Empty. This directory exists so the rest of the repo can reference it
(see `apps/api/internal/gen/`, the CI `proto` job, and the `proto-gen`
Make target). No `.proto` files yet.

## Phase 1+ plan

### Layout

```
packages/proto/
├── buf.yaml                    # buf module config
├── buf.gen.golang.yaml         # Go codegen -> apps/api/internal/gen/
├── buf.gen.python.yaml         # Python codegen -> packages/contracts/src/orpheus_contracts/gen/
└── orpheus/
    └── v1/
        ├── jobs.proto          # Job lifecycle, status, result references
        ├── artifacts.proto     # Audio upload, derived outputs (transcripts, stems)
        ├── workflows.proto     # Long-running workflow RPCs (e.g. transcribe-long)
        └── common.proto        # Pagination, errors, tenant ids
```

### Codegen

We use [`buf`](https://buf.build) as the protobuf toolchain. Two gen
configs target the two language workspaces:

| Language | Output path                                          | Plugin           |
|----------|------------------------------------------------------|------------------|
| Go       | `apps/api/internal/gen/orpheus/v1/`                  | `bufbuild/go`    |
| Python   | `packages/contracts/src/orpheus_contracts/gen/`      | `bufbuild/python` (betterproto or pydantic) |

Generated code is **checked in**, not regenerated in CI. The `proto`
job in `.github/workflows/ci.yml` runs `buf lint` and `buf breaking`
against `main`; a `make proto-gen` target regenerates the stubs locally.

### Why a dedicated package?

`packages/contracts/` is the home for Python-side contracts (OpenAPI,
AsyncAPI, Pydantic models). The Protobuf surface is its own thing —
it's the wire format and is consumed by both the Go API and the Python
workers, so it lives at the workspace root, not inside `contracts/`.

### References

- `docs/architecture/PRODUCTION_DESIGN.md` §6 (Service topology) and
  §8 (Inter-service communication) — the canonical justification.
- `docs/adr/` — will gain an ADR pinning the choice of `buf` over
  raw `protoc` in Phase 1.
