# Runbook — Outbox not draining

**Alerts:** `OutboxPublishErrors`, `OutboxStalled`, `OutboxPublishSlow`
**SLO:** [SLO 4 — event/webhook delivery](../SLOs.md)
**Dashboard:** Orpheus / Database & RLS (outbox latency panel), Orpheus / Cost & Usage (events published)
**Severity:** page

## Background

Orpheus uses a **transactional outbox**: the API writes domain events to an
outbox table in the same DB transaction as the business change, and a
background publisher loop reads pending rows and publishes them to NATS
(`apps/api/internal/outbox/publisher.go`). If the publisher stalls, events
are durably queued in the DB but not delivered — no data loss yet, but
delivery SLO burns and consumers/webhooks fall behind.

## Symptom

- `orpheus_outbox_published_total{result="error"}` climbing (publish errors).
- **Stalled:** jobs are being submitted but `result="success"` publishes are
  flat at zero.
- Publish p95 latency climbing above 1s.

## Queries

Publish success vs error rate:

```promql
sum by (result) (rate(orpheus_outbox_published_total[5m]))
```

Stall detector (submitted but nothing published):

```promql
sum(rate(orpheus_jobs_submitted_total[15m]))
and
sum(rate(orpheus_outbox_published_total{result="success"}[15m])) == 0
```

Publish latency p95 by event type:

```promql
histogram_quantile(0.95,
  sum by (le, event_type) (rate(orpheus_outbox_publish_duration_seconds_bucket[5m])))
```

Backlog depth (direct from DB — the metrics only see attempts, not the
queue length):

```sql
-- pending, oldest-first: how deep and how old is the backlog?
SELECT count(*) AS pending,
       min(created_at) AS oldest,
       now() - min(created_at) AS lag
FROM outbox
WHERE published_at IS NULL;

-- rows stuck retrying
SELECT event_type, count(*), max(attempts) AS max_attempts
FROM outbox
WHERE published_at IS NULL AND attempts > 0
GROUP BY event_type
ORDER BY 2 DESC;
```

## Likely causes & remediation

| Cause | Signal | Fix |
| ----- | ------ | --- |
| NATS down / unreachable | publish errors, NATS `up` down | Restore NATS (`orpheus-nats`); check `http://nats:8222/healthz`. Publisher retries automatically once reconnected. |
| JetStream stream missing | errors mention no stream/subject | The API creates `ORPHEUS_JOBS` on startup (`EnsureStream`). Restart the API or re-run stream setup. |
| Publisher loop wedged | stalled: submits > 0, success == 0, no errors | Restart the API pod running the publisher. Confirm the loop resumes (success rate > 0). |
| DB read pressure | publish latency high, DB slow | See [postgres-connection-exhaustion](postgres-connection-exhaustion.md); the publisher competes for connections. |
| Poison event | one `event_type` stuck with high `attempts` | Inspect the row payload; if malformed, quarantine (move/park) that row so the loop can drain the rest, then fix + replay. |

## Exit criteria

- `result="success"` publish rate recovered and tracking the submit rate.
- Backlog (`published_at IS NULL`) draining toward zero, oldest `lag` shrinking.
- No `result="error"` for 15 min.

## Related

If events publish fine but **subscribers/webhooks** aren't receiving them,
this is a delivery problem downstream of the outbox — see
[webhook-delivery-failing](webhook-delivery-failing.md).
