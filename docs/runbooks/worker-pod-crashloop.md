# Runbook — Worker pod crashloop

**Alerts:** `WorkersDown`, `WorkerJobFailureRate`, `WorkerProcessingSlow`,
`QueueDepthGrowing`
**SLO:** [SLO 3 — job success rate](../SLOs.md)
**Dashboard:** Orpheus / Workers & Queue (`orpheus-workers-queue`)
**Severity:** page

## Background

The Python workers run three logical processes (control plane, gRPC, worker).
In dev they run in one container (`orpheus-workers-all`); in prod-style
deployment they split (`--profile split`). The worker consumes JetStream
messages and processes jobs; metrics live in
`apps/workers/src/orpheus_workers/metrics.py`.

## Symptom

- `up{job=~"orpheus-workers.*"} == 0` — scrape target gone.
- Pod restart count climbing / `CrashLoopBackOff`.
- Job failure rate up, queue backlog growing while workers flap.

## First 5 minutes

1. Confirm which process is failing (control / grpc / worker) and how many
   replicas are affected.
2. Look at the **exit reason** — OOMKilled vs non-zero exit vs failed
   liveness probe are very different problems.
3. Check whether jobs are backing up (below) so you know the urgency.

## Queries

Target availability + restart pattern:

```promql
up{job=~"orpheus-workers.*"}
```

```bash
# Kubernetes
kubectl get pods -l app=orpheus-workers
kubectl describe pod <pod>            # Last State, Reason (OOMKilled?), Exit Code
kubectl logs <pod> --previous        # logs from the crashed instance

# docker-compose (dev)
docker compose ps orpheus-workers-all
docker compose logs --tail=200 orpheus-workers-all
docker inspect orpheus-workers-all --format '{{ "{{ .State.OOMKilled }} {{ .RestartCount }}" }}'
```

Failure + backlog signals:

```promql
sum(rate(orpheus_jetstream_messages_total{result=~"nak|term|parse_error"}[5m]))
  / clamp_min(sum(rate(orpheus_jetstream_messages_total[5m])), 1e-9)      # failure ratio
sum(rate(orpheus_jobs_submitted_total[10m])) - sum(rate(orpheus_jobs_processed_total[10m]))  # backlog growth
histogram_quantile(0.95, sum by (le, processor) (rate(orpheus_job_processing_duration_seconds_bucket[10m])))
```

Error logs in **Loki**:

```logql
{service_name="orpheus-workers"} | json | level="error"
```

## Likely causes & remediation

| Cause | Signal | Fix |
| ----- | ------ | --- |
| OOMKilled | `Reason: OOMKilled`, exit 137 | Raise memory limit or fix the leak/large-payload handler. Cap concurrency. |
| Bad deploy / import error | crashes immediately on start, stack trace in logs | Roll back image. |
| Missing/bad config | fails connecting to NATS/DB/S3 on boot | Verify `ORPHEUS_WORKER_*` env (NATS_URL, DATABASE_URL, REDIS_URL, S3_*). Check the dependency is reachable. |
| Dependency down | boots then dies on first use | Restore Postgres/NATS/Redis/MinIO; workers recover on reconnect. |
| Poison message | one processor crashes on a specific payload | Identify via `parse_error`/`term`; park the message, ship a fix, replay. |
| Liveness probe too tight | killed while healthy-but-slow | Loosen probe timeout / startupProbe. |
| Under-provisioned | no crash but backlog grows, processing slow | Scale replicas out; investigate slow processor. |

## Stop-the-bleeding

- If a **poison message** is crashing every replica, the whole consumer
  crashloops. Park/term that message on JetStream so the fleet can make
  progress, then fix and replay.
- If it's **capacity**, scale replicas up before chasing root cause — the
  backlog is customer-visible latency.

## Exit criteria

- All worker targets `up == 1`, restart count stable.
- Failure ratio back to baseline, backlog growth ≤ 0 (draining).
- p95 processing time back under its per-processor target.
