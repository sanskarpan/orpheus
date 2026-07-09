# ADR-0004: Arq for Simple Jobs, Temporal for Workflows

- **Status:** Accepted
- **Date:** 2026-07-09

## Context

Our job shapes span "fire-and-forget metadata extraction" (sub-second) to
"transcribe hour-long audio, chunk, parallel transcribe, stitch, persist"
(minutes to hours). One queue system is not enough; two is correct when
the boundary is sharp.

## Decision

- **Arq** (Redis-based) for single-activity jobs: < 60s, no signals, no
  humans, no saga, no fan-out.
- **Temporal** for everything else: multi-step, days/weeks, human-in-loop,
  saga-shaped.

The decision rule, codified:

```
if job is single-activity, no signals, no humans, no saga, < 60s, no fan-out:
    use ARQ
else:
    use TEMPORAL
```

Phase 4+ uses Temporal Cloud (managed) to avoid the operational cost of
self-hosting the 5+ Temporal services. Reassess at $1M ARR.

## Consequences

- Two mental models, two dashboards, two SDKs.
- Operational cost of Temporal (self-hosted) is real; for v1, we use
  Temporal Cloud.
- Reversibility: medium. Could collapse to one if we never add a real
  workflow.

## Alternatives Considered

- **Celery** — older, less expressive retry, weaker observability.
- **Dramatiq** — good alternative, RabbitMQ-native.
- **Kafka** — heavier; reserve for event sourcing / CDC in v2.
- **NATS JetStream** — lightweight, durable. Strong alternative.

## References

- `docs/architecture/PRODUCTION_DESIGN.md` §3.4 (Message Broker), §5.4
  (Job orchestration rule)
- Temporal, [Why Temporal](https://temporal.io/blog/why-temporal)
