# ADR-0012: Monorepo with uv + pnpm Workspaces

- **Status:** Accepted
- **Date:** 2026-07-09

## Context

Code is split across Python (api, workers, sdks) and TypeScript
(web, sdks). Multiple repos add coordination overhead, CI complexity, and
make cross-cutting refactors painful.

## Decision

**One repo**, with `apps/` for services and `packages/` for shared
libraries.

- **Python:** uv workspaces (Rust-based, very fast).
- **TypeScript:** pnpm workspaces + Turborepo for the task graph.

## Consequences

- Atomic cross-app refactors.
- Single CI, single onboarding.
- Single source of truth for ADRs and design docs.
- Repo size grows; mitigated by sparse checkouts and path-based CI
  filters.
- Reversibility: medium (splitting a monorepo is mechanical if needed).

## Alternatives Considered

- **Polyrepo** — adds coordination overhead, makes cross-cutting
  refactors painful.
- **Pants / Bazel** — too heavy for our team size; Python support is
  good but TS support is immature.
- **Nx / Turborepo** for both languages — Turborepo doesn't manage
  Python dependencies; we use uv for that anyway.

## References

- `docs/architecture/PRODUCTION_DESIGN.md` §3.13 (CI/CD), §14.1 (Test
  pyramid)
- uv workspaces, [https://docs.astral.sh/uv/concepts/projects/workspaces](https://docs.astral.sh/uv/concepts/projects/workspaces)
