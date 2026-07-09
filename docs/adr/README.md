# ADR Index

Each ADR is a short summary of a significant design decision. Full
context and reasoning lives in
[`docs/architecture/PRODUCTION_DESIGN.md`](../architecture/PRODUCTION_DESIGN.md).

| # | Title | Status | Date |
|---|---|---|---|
| [0001](0001-modular-monolith.md) | Modular Monolith over Microservices | Accepted | 2026-07-09 |
| [0002](0002-fastapi.md) | FastAPI for the Python Worker Control Plane (API tier is Go, see ADR-0013) | Accepted | 2026-07-09 |
| [0003](0003-postgres-rls.md) | PostgreSQL 16 with Row-Level Security for Multi-Tenancy | Accepted | 2026-07-09 |
| [0004](0004-arq-temporal.md) | Arq for Simple Jobs, Temporal for Workflows | Accepted | 2026-07-09 |
| [0005](0005-model-versioning.md) | Model Versioning is First-Class | Accepted | 2026-07-09 |
| [0006](0006-s3-presigned.md) | S3 Multipart Upload via Presigned URLs (No Bytes Through API) | Accepted | 2026-07-09 |
| [0007](0007-keycloak.md) | Keycloak for Auth in v1 | Accepted | 2026-07-09 |
| [0008](0008-gvisor-sandbox.md) | gVisor Sandbox for Untrusted Audio Processing | Accepted | 2026-07-09 |
| [0009](0009-gitops-argocd.md) | GitOps with ArgoCD | Accepted | 2026-07-09 |
| [0010](0010-observability.md) | Self-Hosted Observability Stack, Not Datadog | Accepted | 2026-07-09 |
| [0011](0011-cost-targets.md) | Cost Targets | Accepted | 2026-07-09 |
| [0012](0012-monorepo.md) | Monorepo with uv + pnpm Workspaces | Accepted | 2026-07-09 |
| [0013](0013-polyglot.md) | Polyglot — Go API Tier + Python Worker Tier | Accepted | 2026-07-09 |

## Conventions

- One ADR per significant decision. If a decision touches more than one
  area, write one ADR per concern.
- Status values: `Proposed`, `Accepted`, `Deprecated`, `Superseded by
  ADR-XXXX`.
- ADRs are immutable once Accepted. To change a decision, write a new
  ADR that supersedes the old one.
- Use [0000-template.md](0000-template.md) for new ADRs.
