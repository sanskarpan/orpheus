# ADR-0011: Cost Targets

- **Status:** Accepted
- **Date:** 2026-07-09

## Context

Without an explicit cost target, infra spend grows with usage and no one
notices. Without a margin target, the business dies. We need both, and
they must be visible to every engineer.

## Decision

Track and alert on:

- **≤ $0.10 / MAU / mo infra at 1M MAU.** Alert if it trends above 0.15.
- **≥ 30% gross margin** on each tier (Free, Pay-as-you-go, Startup,
  Enterprise). Alert if any tier drops below 20%.

Implementation:
- Per-tenant metering (CPU-seconds, GPU-seconds, storage GB-days, egress
  GB, webhook deliveries).
- Per-job cost recording in `job_costs` table.
- Cost dashboard per tenant, per processor.
- Kubecost for K8s-native cost breakdown.
- Monthly FinOps review.
- Anomaly detection: daily cost > 1.5× 7-day rolling → Slack; > 2× 30-day
  → page.

## Consequences

- Forces cost-aware decisions at every layer (GPU spot, S3 tiering,
  cardinality discipline).
- Aligns engineering and finance on a shared metric.
- Pricing model must support the margin floor.

## Alternatives Considered

- **No cost target** — default in most startups; rejected. The
  alternative to a cost target is surprise bills.
- **Cost as a soft signal** — easy to ignore. Rejected.

## References

- `docs/architecture/PRODUCTION_DESIGN.md` §19 (Cost Considerations)
