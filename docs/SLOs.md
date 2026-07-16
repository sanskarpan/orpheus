# Orpheus — Service Level Objectives

This document defines the SLOs for Orpheus, the indicators (SLIs) that
measure them, their error budgets, and how the budget governs our response.
All SLIs are derived from metrics the services already emit
(`apps/api/internal/metrics/metrics.go`, `apps/workers/src/orpheus_workers/metrics.py`)
and are graphed on the Grafana dashboards under the **Orpheus** folder.

## Definitions

- **SLI** — a measured ratio of "good" events to "valid" events.
- **SLO** — the target for that ratio over a rolling window.
- **Error budget** — `1 - SLO`. The allowed amount of unreliability. When
  the budget is spent, we stop shipping risky changes and prioritize
  reliability work until it recovers.
- **Window** — all SLOs are measured over a rolling **30 days** unless noted.

## Burn-rate alerting policy

We alert on **error-budget burn rate**, not raw thresholds, using
multi-window multi-burn-rate alerts (see `monitoring/prometheus/alerts.yml`):

| Burn rate | Budget consumed | Long window | Short window | Severity |
| --------- | --------------- | ----------- | ------------ | -------- |
| 14.4x     | 2% in ~1h       | 1h          | 5m           | page     |
| 6x        | 5% in ~6h       | 6h          | 30m          | page     |
| 1x        | slow drift      | 3d          | 6h           | ticket   |

A page fires only when **both** windows are hot, which suppresses flapping
on brief spikes.

---

## SLO 1 — API availability

- **SLI:** fraction of HTTP requests that do **not** return 5xx.
- **Good:** `orpheus_http_requests_total{status!~"5.."}`
- **Valid:** `orpheus_http_requests_total`
- **Target:** **99.9%** over 30 days.
- **Error budget:** 0.1% → ~43m 12s of full outage / 30 days.

```promql
1 - (
  sum(rate(orpheus_http_requests_total{status=~"5.."}[30d]))
  /
  sum(rate(orpheus_http_requests_total[30d]))
)
```

Note: 4xx responses are **client** errors and count as "good" for the
availability SLI (the service did its job). Track them separately for
product/UX health, not availability.

Alerts: `ApiHighErrorRate`, `ApiErrorBudgetFastBurn`, `ApiDown`.
Runbook: [`runbooks/api-5xx-spike.md`](runbooks/api-5xx-spike.md).

---

## SLO 2 — API latency

- **SLI:** fraction of requests served faster than the latency target.
- **Target:** **99%** of requests with `p99 < 1s`, measured per-route and
  overall, over 30 days. Read-heavy routes (`GET /v1/jobs/*`) additionally
  target `p95 < 300ms`.
- **Error budget:** 1% of requests may exceed the threshold.

```promql
histogram_quantile(
  0.99,
  sum by (le, route) (rate(orpheus_http_request_duration_seconds_bucket[5m]))
)
```

Alerts: `ApiHighLatencyP99`.
Runbook: [`runbooks/api-5xx-spike.md`](runbooks/api-5xx-spike.md) (latency section).

---

## SLO 3 — Job success rate

- **SLI:** fraction of processed jobs that complete successfully.
- **Good:** `orpheus_jobs_processed_total{status="completed"}`
- **Valid:** `orpheus_jobs_processed_total` (completed + failed)
- **Target:** **99%** over 30 days.
- **Error budget:** 1% of jobs may end in `failed`.

```promql
sum(rate(orpheus_jobs_processed_total{status="completed"}[30d]))
/
sum(rate(orpheus_jobs_processed_total[30d]))
```

We also track **queue health** as a leading indicator: submitted vs
processed rate. Sustained `submitted > processed` means the backlog is
growing even if the success ratio looks fine.

```promql
sum(rate(orpheus_jobs_submitted_total[10m]))
- sum(rate(orpheus_jobs_processed_total[10m]))
```

Alerts: `WorkerJobFailureRate`, `QueueDepthGrowing`, `WorkerProcessingSlow`.
Runbook: [`runbooks/worker-pod-crashloop.md`](runbooks/worker-pod-crashloop.md).

---

## SLO 4 — Event / webhook delivery

Domain events flow through the transactional **outbox** to NATS, and from
there to webhook subscribers. The delivery SLO covers the outbox publish
step, which is what the API instruments directly.

- **SLI:** fraction of outbox publish attempts that succeed.
- **Good:** `orpheus_outbox_published_total{result="success"}`
- **Valid:** `orpheus_outbox_published_total` (success + error)
- **Target:** **99.9%** of events published within **60s** of being written,
  over 30 days.
- **Error budget:** 0.1% of events may fail to publish or exceed the 60s
  freshness target.

```promql
sum(rate(orpheus_outbox_published_total{result="success"}[30d]))
/
sum(rate(orpheus_outbox_published_total[30d]))
```

Freshness / latency of the publish step:

```promql
histogram_quantile(
  0.95,
  sum by (le) (rate(orpheus_outbox_publish_duration_seconds_bucket[5m]))
)
```

Alerts: `OutboxPublishErrors`, `OutboxStalled`, `OutboxPublishSlow`.
Runbooks: [`runbooks/outbox-not-draining.md`](runbooks/outbox-not-draining.md),
[`runbooks/webhook-delivery-failing.md`](runbooks/webhook-delivery-failing.md).

---

## Error-budget policy (what we DO with the budget)

| Budget remaining | Action |
| ---------------- | ------ |
| > 50%            | Ship normally. Take reliability risks within reason. |
| 10–50%           | Ship with caution. Require review for risky changes. |
| < 10%            | Feature freeze on the affected service. Only reliability + safe changes until the budget recovers over the trailing window. |
| Exhausted        | Incident. Halt non-critical deploys, prioritize root-cause + guardrails, run a retro. |

Budgets are evaluated per-SLO. Exhausting the API-availability budget does
not freeze worker feature work, and vice versa.

## Ownership & review

- SLOs are reviewed quarterly and after any Sev-1/Sev-2 incident.
- Each SLO maps to at least one alert and one runbook (linked above).
- Dashboards: **Orpheus / API**, **Orpheus / Workers & Queue**,
  **Orpheus / Database & RLS**, **Orpheus / Cost & Usage**.
