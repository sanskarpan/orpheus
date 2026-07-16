# Runbook — Postgres connection exhaustion

**Alerts:** surfaces as `ApiHighErrorRate` / `ApiHighLatencyP99` /
`OutboxPublishSlow` (Postgres has no direct alert here yet — add one when the
pgx pool exporter lands).
**SLO:** [SLO 1 — availability, SLO 2 — latency](../SLOs.md)
**Dashboard:** Orpheus / Database & RLS (connection pool panel)
**Severity:** page

## Background

Postgres has a hard `max_connections` ceiling (default 100 on the
`postgres:16-alpine` dev image). The API and workers each keep a client-side
connection pool. When demand exceeds the pool, requests **queue** for a
connection; when the DB's own ceiling is hit, new connections are **refused**
outright (`FATAL: sorry, too many clients already`). Both show up as latency
first, then 5xx.

## Symptom

- API latency climbing, then 5xx.
- Logs: `too many clients already`, `connection pool exhausted`,
  `context deadline exceeded` acquiring a connection.
- Client pool "waiting/blocked" count pinned at the pool max.

## Queries

Pool saturation (Go `database/sql` or pgx pool collectors on the API):

```promql
sum(go_sql_open_connections{job="orpheus-api"})          # in use + idle
sum(go_sql_max_open_connections{job="orpheus-api"})      # ceiling
sum(go_sql_blocked_queries{job="orpheus-api"})           # waiting for a conn
# pgxpool equivalents if that's the driver:
# pgxpool_acquired_conns / pgxpool_max_conns / pgxpool_empty_acquire_total
```

Correlate the symptom:

```promql
histogram_quantile(0.99, sum by (le) (rate(orpheus_http_request_duration_seconds_bucket[5m])))
sum(rate(orpheus_http_requests_total{status=~"5.."}[5m]))
```

Inside Postgres — who is holding connections:

```sql
-- how close to the ceiling
SELECT count(*) AS total,
       (SELECT setting::int FROM pg_settings WHERE name='max_connections') AS max_conn
FROM pg_stat_activity;

-- connections by state and application
SELECT application_name, state, count(*)
FROM pg_stat_activity
GROUP BY 1, 2 ORDER BY 3 DESC;

-- long-idle-in-transaction connections (the usual culprit: they hold a
-- connection AND locks without doing work)
SELECT pid, state, now() - state_change AS idle_for, query
FROM pg_stat_activity
WHERE state = 'idle in transaction'
ORDER BY idle_for DESC;

-- longest-running queries
SELECT pid, now() - query_start AS runtime, state, query
FROM pg_stat_activity
WHERE state <> 'idle'
ORDER BY runtime DESC
LIMIT 20;
```

## Likely causes & remediation

| Cause | Signal | Fix |
| ----- | ------ | --- |
| Leaked / idle-in-transaction conns | many `idle in transaction` for minutes | Find the code path not committing/rolling back. Short-term: terminate the oldest offenders (below). |
| Pool too small for load | blocked/waiting pinned at max, DB not at ceiling | Raise `MaxOpenConns` on the API pool; scale DB if needed. |
| Traffic spike | request rate up, pool + DB both saturated | Scale API out cautiously (more replicas = more connections!), add rate limiting, consider PgBouncer. |
| Too many replicas × pool size | `total` near `max_connections`, many small pools | Put **PgBouncer** in front (transaction pooling) so app pools multiplex; reduce per-pod pool size. |
| Slow queries holding conns | long `runtime` in `pg_stat_activity` | Fix/index the query; add `statement_timeout`. |
| Migrations / batch job | one app_name hogging connections | Throttle the batch; run migrations with a bounded pool. |

Terminate a specific offender (surgical, last resort):

```sql
-- cancel a running query (gentle)
SELECT pg_cancel_backend(<pid>);
-- kill the connection (forceful)
SELECT pg_terminate_backend(<pid>);

-- reap idle-in-transaction sessions older than 5 min
SELECT pg_terminate_backend(pid)
FROM pg_stat_activity
WHERE state = 'idle in transaction'
  AND now() - state_change > interval '5 minutes';
```

## Prevention

- Set `statement_timeout` and `idle_in_transaction_session_timeout` so stuck
  sessions self-heal.
- Size pools deliberately: `sum(per-pod pool) < max_connections` with
  headroom for migrations, psql, and monitoring.
- Introduce **PgBouncer** (transaction mode) before scaling API replicas past
  the point where `replicas × pool_size` approaches `max_connections`.
- Add a pgx/sql pool exporter and a `DbPoolNearExhaustion` alert
  (`waiting > 0 for 5m` OR `open / max > 0.9`).

## Exit criteria

- `open / max` back under ~70%, blocked/waiting at 0.
- `pg_stat_activity` total comfortably below `max_connections`.
- API latency + error rate recovered.
