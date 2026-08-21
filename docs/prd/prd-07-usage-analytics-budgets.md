# PRD 07 — Per-tenant API usage analytics + budget alerts

**Status:** Draft · **Owner:** Billing · **Reviewers:** Platform, Notifications, Finance
**Related:** PRODUCTION_DESIGN §8.3 (`GET /v1/usage`), §9.1 (`usage_events`, `job_costs`),
§9.3(hot query 5: hourly usage rollup) · existing `Usage`/`UsageBreakdown`/`UsagePeriod` schemas

## 1. Problem & motivation

`GET /v1/usage` returns a single current-period summary (jobs count, GPU-seconds, storage,
egress, total USD, breakdown). That answers "how much this month" but not "how did usage trend,"
"which processor dominates cost," or "warn me before I blow my budget." IMPLEMENTATION_STATUS
notes cost attribution is only partially wired (`cost_usd` column exists, not computed). Tenants
managing spend need **time-series analytics** and **budget alerts** so surprise bills and runaway
jobs are caught early — and so we can enforce spend caps for abuse protection.

## 2. Goals / non-goals

**Goals**
- Time-series usage/cost, grouped by day/hour and by dimension (processor, job status, api-key).
- **Budgets** per org (and optionally per api-key/processor) with threshold alerts (e.g. 50/80/100%).
- Alerts delivered via webhook (`usage.budget_threshold`) and email; optional **hard cap** that
  rejects new jobs once exceeded.

**Non-goals**
- Invoicing/payment (Stripe integration is separate).
- Cross-org / marketplace revenue analytics.
- Real-time per-request metering dashboards (rollups are near-real-time, minute-level).

## 3. User stories

- As a tenant admin, I want a daily cost chart broken down by processor for the last 30 days.
- As a tenant admin, I want an email + webhook when I hit 80% of my $2,000/month budget.
- As a platform operator, I want a hard cap so a buggy client loop can't run up an unbounded bill.

## 4. Proposed API / UX

```jsonc
// Time-series analytics (extends existing /v1/usage)
GET /v1/usage/timeseries?granularity=day&from=2026-06-15&to=2026-07-15
    &group_by=processor              // processor | status | api_key
// → { series: [ { bucket: "2026-07-14", processor: "orpheus.audio.transcribe",
//                 jobs: 120, gpu_seconds: 3600, cost_usd: 4.80 }, ... ] }

// Budgets
POST   /v1/budgets
{ "scope": "org",                     // org | api_key | processor
  "scope_id": null, "period": "monthly", "limit_usd": 2000,
  "alert_thresholds": [0.5, 0.8, 1.0], "enforcement": "alert" }  // alert | hard_cap
GET    /v1/budgets            // list + current spend + % consumed
PATCH  /v1/budgets/{id}
DELETE /v1/budgets/{id}
```

Alerts fire as signed webhook `usage.budget_threshold` (`{budget_id, threshold, spend_usd,
limit_usd}`) + email. With `enforcement=hard_cap`, new `POST /v1/jobs` beyond the cap return
`402`/`429` Problem `type=budget-exceeded` (in-flight jobs finish).

## 5. Data-model changes

Reuse `usage_events` + `job_costs`; add rollup + budget tables (org-scoped, RLS, partitioned):

```
usage_rollup_hourly( org_id, hour, dimension, dimension_value,
    jobs int, gpu_seconds numeric, cpu_seconds numeric, storage_gb_days numeric,
    egress_gb numeric, cost_usd numeric, primary key(org_id,hour,dimension,dimension_value) )
-- populated by the existing hourly rollup pattern (PRODUCTION_DESIGN §9.3 query 5)

budgets( id, org_id, scope, scope_id, period, limit_usd,
    alert_thresholds numeric[], enforcement text, created_at, updated_at )

budget_alerts( id, org_id, budget_id, threshold, spend_usd, fired_at )   -- dedupe per period+threshold
```

- **Prerequisite:** compute `job_costs`/`cost_usd` from CPU/GPU/memory-seconds at `persist_result`
  (currently a gap). This PRD depends on that landing.
- Analytics reads route to the **read replica** (§9.4) to protect the write path.

## 6. Edge cases & security

- **Tenant isolation:** all rollups/budgets RLS-scoped by `org_id`; cross-org aggregation is only
  available to internal admin roles, never tenant-facing.
- **Cardinality:** do NOT label Prometheus metrics with `tenant_id` (§10.9); tenant analytics live
  in Postgres rollups, not the metrics system.
- **Enforcement races:** hard-cap check is best-effort at submit (rollup lag ≤ minutes); document
  that a small overshoot is possible; never retroactively fail completed jobs.
- **Alert storms:** dedupe alerts per `(budget, period, threshold)` so each threshold fires once.
- **Abuse:** hard-cap doubles as a runaway-loop and cost-DoS guard; rate limits still apply independently.

## 7. Metrics / SLAs

- Rollup freshness `< 5 min` behind real time; `budget_alert_latency < 2 min` after crossing.
- `usage_timeseries_query_p95 < 500ms` (replica-served).

## 8. Rollout plan

1. Land cost computation into `job_costs` (dependency).
2. Ship hourly rollup + `/v1/usage/timeseries`.
3. Ship budgets with `enforcement=alert` (webhook + email).
4. Ship `enforcement=hard_cap` behind a per-org flag; enable for abuse-prone plans.

## 9. Open questions

- Cost model inputs: authoritative $/GPU-second and $/CPU-second per instance class (Finance).
- Hard-cap default: reject with `402` (billing) vs `429` (rate) — pick one for SDK clarity.
- Budget period reset semantics (calendar month vs. rolling 30 days vs. billing anchor).
