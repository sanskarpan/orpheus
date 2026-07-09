# ADR-0010: Self-Hosted Observability Stack, Not Datadog

- **Status:** Accepted
- **Date:** 2026-07-09

## Context

Observability is the silent cost killer (cardinality explosion). At
1M-MAU scale, managed observability is $20–40k/mo. Self-hosting is
~$1.5–2.5k/mo but requires SRE capacity.

## Decision

**OpenTelemetry** as the SDK. Self-host:

- **Prometheus + Mimir** for metrics
- **Loki** for logs
- **Tempo** for traces
- **Grafana** for dashboards and alerts
- **Pyroscope** for continuous profiling
- **Alertmanager** → PagerDuty + Slack

OpenTelemetry SDKs are used in both the Go API tier
(`go.opentelemetry.io/otel`) and the Python worker tier
(`opentelemetry-python`). Trace context is propagated across the gRPC
boundary via W3C Trace Context headers, so a single trace spans an API
request → gRPC call → worker activity → Temporal activity.

Cardinality discipline is enforced in CI: `tenant_id`, `user_id`,
`job_id` are banned as Prometheus labels.

## Consequences

- ~5× cheaper at scale than Datadog.
- Predictable cost.
- Requires SRE capacity to operate.
- Switching to Grafana Cloud is straightforward; switching to Datadog
  is not.

## Alternatives Considered

- **Datadog** — best DX, highest cost.
- **New Relic** — similar trade-off.
- **Grafana Cloud** — middle ground (managed Grafana stack, lower ops
  burden).

## References

- `docs/architecture/PRODUCTION_DESIGN.md` §13 (Observability)
- OpenTelemetry, [https://opentelemetry.io](https://opentelemetry.io)
