# Runbook — API 5xx spike / high latency

**Alerts:** `ApiHighErrorRate`, `ApiRouteHighErrorRate`,
`ApiErrorBudgetFastBurn`, `ApiHighLatencyP99`, `ApiDown`
**SLOs:** [SLO 1 — availability, SLO 2 — latency](../SLOs.md)
**Dashboard:** Orpheus / API (`orpheus-api`)
**Severity:** page

## Symptom

- 5xx error ratio above 2% (or a single route above 5%).
- p99 latency above 1s.
- `up{job="orpheus-api"} == 0` (target unreachable).

## First 5 minutes (triage)

1. Open the **Orpheus / API** dashboard. Confirm the blast radius:
   global vs single-route, and whether it correlates with a latency jump.
2. Is it all instances or one? Check `up{job="orpheus-api"}`.
3. Did a deploy just land? Correlate the spike start with the release
   timeline. If yes, **roll back first, investigate after**.

## Queries

Global 5xx ratio:

```promql
sum(rate(orpheus_http_requests_total{status=~"5.."}[5m]))
/ clamp_min(sum(rate(orpheus_http_requests_total[5m])), 1e-9)
```

Which route is failing:

```promql
topk(5,
  sum by (route) (rate(orpheus_http_requests_total{status=~"5.."}[5m]))
  / clamp_min(sum by (route) (rate(orpheus_http_requests_total[5m])), 1e-9)
)
```

p99 latency by route:

```promql
histogram_quantile(0.99,
  sum by (le, route) (rate(orpheus_http_request_duration_seconds_bucket[5m])))
```

Correlate with dependencies (DB pool, outbox, RLS):

```promql
sum(rate(orpheus_rls_denials_total[5m]))                       # authz regressions
histogram_quantile(0.95, sum by (le) (rate(orpheus_outbox_publish_duration_seconds_bucket[5m])))
```

Pull traces for a failing route in **Tempo** (filter `status=error`,
`service.name="orpheus-api"`) and jump to correlated logs in **Loki**
(`{service_name="orpheus-api"} | json | level="error"`).

## Likely causes & remediation

| Cause | Signal | Fix |
| ----- | ------ | --- |
| Bad deploy | spike starts at release time | Roll back to previous image/tag. |
| DB unavailable / pool exhausted | latency up, DB errors in logs, blocked conns climbing | See [postgres-connection-exhaustion](postgres-connection-exhaustion.md). |
| Downstream (NATS/S3/Keycloak) down | errors on routes that publish/auth | Check that dependency's health; the outbox will buffer events, auth failures return 5xx if Keycloak JWKS is unreachable. |
| One poison route | single route hot in `topk` | Feature-flag / disable that route; ship targeted fix. |
| Overload | request rate spike + latency up + CPU saturated | Scale out API replicas; enable/tighten rate limits. |
| Target down | `up == 0`, crashloop | Check pod logs/exit codes; if OOM, raise memory limit. |

## Exit criteria

- 5xx ratio back under 1% for 15 min and p99 under 1s.
- Confirm the error budget burn has stopped (`ApiErrorBudgetFastBurn`
  resolved). File a follow-up if budget was materially consumed.
