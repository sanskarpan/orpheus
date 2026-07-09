# ADR-0001: Modular Monolith over Microservices

- **Status:** Accepted
- **Date:** 2026-07-09

## Context

We are a 5–15 engineer team pre-product-market-fit. Service boundaries are
typically wrong on day one and the microservice tax (deploys, observability,
distributed tracing, contract testing, network reliability) is high when you
don't yet know which modules will diverge in scaling profile.

## Decision

Build a single FastAPI codebase with strong internal module boundaries. Two
**deployment units**: `api` and `workers` (added in Phase 2).

Split a module into its own service only when one of these triggers fires:
1. Different scaling profile (e.g., 100× the resources of the monolith)
2. Different deployment cadence (e.g., hourly vs daily)
3. Blast-radius isolation is required (e.g., billing)
4. Data scale forces dedicated storage
5. Org boundary (different team)

## Consequences

- Faster iteration, fewer repos, simpler ops.
- Atomic cross-module refactors.
- All engineers can work in the same codebase; lower cognitive overhead.
- Single CI, single deploy pipeline.
- Risk: monolith becomes a coordination bottleneck when the team grows past 20.

## Alternatives Considered

- **Microservices from day 1** — rejected. Wrong service boundaries on day one
  is more expensive than a clean monolith.
- **Serverless (Lambda + API Gateway)** — rejected for the API tier. Cold
  starts, per-request cost, and 6 MB body limit are wrong for a multimedia
  processing platform.

## References

- `docs/architecture/PRODUCTION_DESIGN.md` §5 (Final System Architecture)
- Sam Newman, *Monolith to Microservices* (2019)
- Simon Wardley, *"Crossing the River by Feeling the Stones"* (2016)
