# Orpheus API (Go)

Public HTTP API for [Orpheus](../../README.md) — a multi-tenant SaaS for
asynchronous audio processing. This is the Go implementation of the API
tier (Phase 0). The Python worker tier is a separate package under
`apps/workers/`.

## Quickstart

```bash
# from this directory (apps/api/)
make run
# or
go run ./cmd/api
```

The service binds to `http://0.0.0.0:8080` by default. Override with
`ORPHEUS_HOST` and `ORPHEUS_PORT`.

## Endpoints

| Method | Path                | Description                                                              |
| ------ | ------------------- | ------------------------------------------------------------------------ |
| GET    | `/health`           | Liveness probe. Returns `{"status":"ok"}` with HTTP 200.                |
| GET    | `/ready`            | Readiness probe. Phase 1+ will fan out to DB / Redis / S3.              |
| GET    | `/metrics`          | Prometheus exposition format (Go runtime + process metrics).            |
| GET    | `/api/openapi.json` | OpenAPI 3.1 specification.                                              |
| GET    | `/api/docs`         | Swagger UI, loads `/api/openapi.json`.                                   |
| GET    | `/api/redoc`        | ReDoc UI, loads `/api/openapi.json`.                                    |

## Layout

```
apps/api/
├── Makefile                    # Go-specific targets (build, test, run, lint)
├── README.md                   # this file
├── .golangci.yml               # golangci-lint v2 config
├── go.mod / go.sum             # module definition
├── api/
│   └── openapi.json            # canonical OpenAPI spec (mirrored below)
├── cmd/
│   └── api/
│       └── main.go             # entry point
└── internal/
    ├── config/                 # envconfig-based config (ORPHEUS_*)
    ├── handlers/               # HTTP handlers + embedded openapi.json
    ├── logging/                # slog setup (JSON in prod, text in dev)
    ├── server/                 # chi router, middleware, graceful shutdown
    └── version/                # build version (overridable via -ldflags -X)
```

## Development

```bash
make tidy     # go mod tidy
make build    # compile binary to bin/api
make test     # go test ./...
make lint     # golangci-lint run
make fmt      # auto-format (gofmt + goimports)
make clean    # remove build artefacts
```

## Configuration

All settings are environment variables, prefixed with `ORPHEUS_`.

| Variable                          | Default        | Notes                                                                 |
| --------------------------------- | -------------- | --------------------------------------------------------------------- |
| `ORPHEUS_ENV`                     | `dev`          | `dev` / `staging` / `prod`. Selects JSON log format in `prod`.        |
| `ORPHEUS_LOG_LEVEL`               | `INFO`         | `DEBUG` / `INFO` / `WARN` / `ERROR` (case-insensitive).               |
| `ORPHEUS_SERVICE_NAME`            | `orpheus-api`  | Service identifier stamped on every log line.                         |
| `ORPHEUS_HOST`                    | `0.0.0.0`      | Bind address.                                                         |
| `ORPHEUS_PORT`                    | `8080`         | Bind port.                                                            |
| `ORPHEUS_SHUTDOWN_GRACE_SECONDS`  | `30`           | How long to wait for in-flight requests on SIGINT / SIGTERM.          |

## Build version

`internal/version.Version` is overridden at link time:

```bash
go build -ldflags "-X github.com/orpheus/api/internal/version.Version=1.2.3" -o bin/api ./cmd/api
```

## OpenAPI spec

The spec is the single source of truth for the HTTP contract. The file at
`internal/handlers/openapi.json` is embedded into the binary at build time
via `//go:embed`. A mirrored copy lives at `api/openapi.json` for tooling
that expects the spec at the project root; keep them in sync.
