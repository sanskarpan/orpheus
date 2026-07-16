# Orpheus Observability Stack

Metrics, logs, and traces for Orpheus (gap #6). Everything here is config —
no application code is modified. The API and workers already expose
Prometheus metrics and can emit OTLP; this stack collects, stores, and
visualizes them.

## Components

| Service | Image | Purpose | Host port |
| ------- | ----- | ------- | --------- |
| Prometheus | `prom/prometheus` | Metrics TSDB, scraper, alert rules | 9090 |
| Grafana | `grafana/grafana` | Dashboards (provisioned) | 3000 |
| Loki | `grafana/loki` | Log aggregation | 3100 |
| Tempo | `grafana/tempo` | Distributed tracing | 3200 |
| OTel Collector | `otel/opentelemetry-collector-contrib` | OTLP ingress → Tempo/Prometheus/Loki | 4317, 4318, 8888, 8889 |

## Data flow

```
                 scrape /metrics
Prometheus ◀───────────────────────── orpheus-api :8080
    ▲  ▲                               orpheus-workers :8081 /:8082
    │  └── scrape ── NATS :8222 (via prometheus-nats-exporter)
    │
    │ remote_write (metrics)
    │
OTel Collector ◀── OTLP (grpc 4317 / http 4318) ── API + workers
    │  ├── traces  ─▶ Tempo
    │  └── logs    ─▶ Loki
    │
Grafana ── datasources ─▶ Prometheus + Loki + Tempo  (trace↔log↔metric linked)
```

## Prerequisites

The app stack must be running first — it creates the shared docker network
these services attach to.

```bash
# from the repo root
docker compose up -d
```

The observability compose file joins the app's default network
(`orpheus_default`) as **external**. If your app stack runs under a different
Compose project name, override it:

```bash
export ORPHEUS_APP_NETWORK=<yourproject>_default
```

## Run it

```bash
# from the repo root
docker compose -f monitoring/docker-compose.observability.yml up -d

# check health
docker compose -f monitoring/docker-compose.observability.yml ps
```

Open:

- Grafana — http://localhost:3000  (admin / admin — dev only)
  Dashboards land under the **Orpheus** folder, already wired to datasources.
- Prometheus — http://localhost:9090  (see **Status → Targets** for scrape health)

Tear down:

```bash
docker compose -f monitoring/docker-compose.observability.yml down       # keep data
docker compose -f monitoring/docker-compose.observability.yml down -v    # wipe TSDB/volumes
```

## Wiring the app to emit traces + logs (OTLP)

Metrics are **scraped** and work out of the box. To get traces and logs into
Tempo/Loki, point the API and workers at the collector. Inside the docker
network:

```bash
OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector:4317   # gRPC
# or HTTP:
OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector:4318
OTEL_EXPORTER_OTLP_PROTOCOL=grpc                         # or http/protobuf
OTEL_SERVICE_NAME=orpheus-api                            # / orpheus-workers
OTEL_RESOURCE_ATTRIBUTES=deployment.environment=dev
```

From the host (e.g. API running locally, stack in docker) use
`http://localhost:4317` / `:4318`.

## Dashboards

Provisioned from `grafana/dashboards/*.json` (source of truth), loaded via
`grafana/provisioning/dashboards/dashboards.yaml`:

- **Orpheus / API** (`api.json`) — request rate, error rate, p50/p95/p99
  latency by route, availability, 5xx ratio.
- **Orpheus / Workers & Queue** (`workers-queue.json`) — submitted vs
  processed, backlog growth, success ratio, JetStream ack/nak, S3 ops.
- **Orpheus / Database & RLS** (`database-rls.json`) — RLS denials by table,
  connection pool, outbox publish latency.
- **Orpheus / Cost & Usage** (`cost-usage.json`) — job volume, compute-seconds
  by processor, events published, traffic by route.

Every panel uses the real metric names from
`apps/api/internal/metrics/metrics.go` and
`apps/workers/src/orpheus_workers/metrics.py`.

## Alerts

Rules live in `prometheus/alerts.yml` (loaded by Prometheus). They cover API
error-rate + latency burn, outbox stall/errors, worker failures, queue depth,
and RLS spikes. Each alert's `runbook` annotation points at
`docs/runbooks/*.md`. Prometheus evaluates them; wire an Alertmanager target
in `prometheus/prometheus.yml` to actually route pages.

Reload rules without a restart:

```bash
curl -X POST http://localhost:9090/-/reload
```

## NATS metrics (follow-up)

The bare `nats` container serves JSON on `:8222`, not Prometheus text. To get
queue-depth/JetStream metrics into Prometheus, deploy
[`prometheus-nats-exporter`](https://github.com/nats-io/prometheus-nats-exporter)
pointed at `http://nats:8222` and repoint the `nats` scrape job at the
exporter's `:7777`. The target is already documented in `prometheus.yml` so
the queue-depth alert has a home the moment the exporter lands.

## Layout

```
monitoring/
├── docker-compose.observability.yml
├── prometheus/
│   ├── prometheus.yml          # scrape configs (API, workers, NATS, collector)
│   └── alerts.yml              # alerting rules
├── otel-collector/
│   └── config.yaml             # OTLP in → Tempo/Prometheus/Loki
├── tempo/config.yaml
├── loki/config.yaml
└── grafana/
    ├── provisioning/
    │   ├── datasources/datasources.yaml
    │   └── dashboards/dashboards.yaml
    └── dashboards/
        ├── api.json
        ├── workers-queue.json
        ├── database-rls.json
        └── cost-usage.json

docs/
├── SLOs.md
└── runbooks/
    ├── api-5xx-spike.md
    ├── outbox-not-draining.md
    ├── webhook-delivery-failing.md
    ├── worker-pod-crashloop.md
    └── postgres-connection-exhaustion.md
```

> Credentials in this stack are **dev-only**. Never use them in production.
