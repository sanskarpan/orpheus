# ADR-0002: FastAPI for the Python Worker Control Plane (API tier is Go, see ADR-0013)

- **Status:** Accepted
- **Date:** 2026-07-09

## Context

We need a Python HTTP framework for the **worker control plane**: a
small internal FastAPI service that lives next to the Python worker
plane and exposes worker-pool health, model-registry status, and admin
endpoints to the Go API tier over gRPC.

Note: this ADR was originally about the public API tier. The polyglot
decision (ADR-0013) moved the public API to Go. This ADR now documents
the Python HTTP framework for the **worker control plane only** — the
FastAPI service that exposes worker-pool health, model registry, and
admin endpoints to the Go API tier over gRPC. The public REST surface
is served by the Go API tier (see §3.2, ADR-0013, ADR-0001).

Some teams may have an existing DRF codebase, but we are not committing
to Django for the new system.

## Decision

Use **FastAPI** on Python 3.12+ with Pydantic v2 for validation and
SQLAlchemy 2.0 (async) for ORM. FastAPI generates OpenAPI 3.1 from the
Pydantic types on each route.

## Consequences

- ~2× throughput vs DRF for our workload shape.
- Async-native, no GIL-bound thread pools to size.
- OpenAPI 3.1 is the lingua franca; codegen works in every language.
- Lose Django admin (replace with a custom admin or buy one).
- Lose Django ORM (use SQLAlchemy 2.0 with asyncpg).
- Migration cost from a small DRF codebase: a few endpoints and models —
  trivial.

## Alternatives Considered

- **Django REST Framework** — synchronous-first, OpenAPI is bolted on via
  drf-spectacular.
- **Litestar** — similar to FastAPI, smaller community. Watch.
- **Flask** — no async, no built-in OpenAPI.

## References

- `docs/architecture/PRODUCTION_DESIGN.md` §3.2.b (Python worker
  control plane), §5.2 (component map), ADR-0013 (polyglot decision)
