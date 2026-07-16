# Runbook — Webhook delivery failing

**Alerts:** `OutboxPublishErrors` (upstream), `WorkersDown` (delivery worker)
**SLO:** [SLO 4 — event/webhook delivery](../SLOs.md)
**Dashboard:** Orpheus / Cost & Usage (events published), Orpheus / Workers & Queue
**Severity:** page (systemic) / ticket (single endpoint)

## Background

Delivery pipeline, end to end:

```
API tx ──▶ outbox table ──▶ publisher ──▶ NATS (ORPHEUS_JOBS)
                                                │
                                                ▼
                                    delivery worker ──▶ customer webhook URL
```

The two failure domains are **upstream** (event never reaches NATS — that's
the [outbox runbook](outbox-not-draining.md)) and **downstream** (event is on
NATS but the customer's HTTP endpoint is failing or the delivery worker is
down). This runbook covers the downstream side.

## Symptom

- Customers report missing webhook notifications.
- Delivery worker error/retry logs climbing.
- A specific customer endpoint returning non-2xx.

## First: is it upstream or downstream?

Confirm events ARE reaching the bus:

```promql
sum by (event_type) (rate(orpheus_outbox_published_total{result="success"}[5m]))
```

- **Flat / zero** → upstream. Go to [outbox-not-draining](outbox-not-draining.md).
- **Healthy** → downstream. Continue here.

## Queries

Delivery worker health (JetStream consume side):

```promql
sum by (result) (rate(orpheus_jetstream_messages_total[5m]))   # ack vs nak/term
sum(rate(orpheus_jobs_processed_total{status="failed"}[5m]))
up{job=~"orpheus-workers.*"}
```

Delivery logs in **Loki** (filter to the delivery worker + errors):

```logql
{service_name="orpheus-workers"} | json | msg =~ "(?i)webhook|delivery" | level="error"
```

JetStream consumer backlog (pending / redelivery) via the NATS monitor:

```bash
# num_pending and num_ack_pending indicate a stuck or slow consumer
curl -s http://nats:8222/jsz?consumers=true | jq '.account_details[].stream_detail[].consumer_detail[]
  | {name, num_pending, num_ack_pending, num_redelivered}'
```

## Likely causes & remediation

| Cause | Signal | Fix |
| ----- | ------ | --- |
| Customer endpoint down/slow | non-2xx or timeouts to one host | Expected: rely on retry/backoff. If it exceeds max attempts, event lands in DLQ — notify customer, replay from DLQ once healthy. |
| Delivery worker down | `up{job=~"orpheus-workers.*"} == 0`, pending climbing | Restart worker; see [worker-pod-crashloop](worker-pod-crashloop.md). |
| Consumer wedged / redelivery storm | `num_redelivered` high, ack_pending stuck | Inspect for a poison message; term/park it, then let the consumer advance. |
| Bad webhook config | 401/404 to customer, signature mismatch | Verify the customer's stored URL + signing secret; re-issue secret if rotated. |
| Global outage | all endpoints failing at once | Likely network egress / DNS from the worker. Check egress + shared HTTP client health. |

## Backoff & DLQ

Deliveries retry with exponential backoff. After max attempts an event is
parked (DLQ). Replaying: fix the endpoint/config, then re-publish the parked
events. Do **not** hand-retry into a still-broken endpoint — you'll just
refill the DLQ.

## Exit criteria

- ack rate recovered, `nak/term` back to baseline.
- Consumer `num_pending` draining.
- DLQ replayed for the affected customer(s); backlog cleared.
