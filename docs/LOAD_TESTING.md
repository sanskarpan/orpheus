# Load testing

The API has an in-process load test at
`apps/api/internal/e2e/loadtest_test.go`. It boots the real server against
the live stack (Postgres/NATS/MinIO), authenticates with a real API key,
and drives a realistic read/write mix (`GET /v1/jobs`, `GET /v1/artifacts`,
`POST /v1/jobs`, `GET /v1/jobs/{id}`) while collecting latency
percentiles, throughput, and error rate.

It is gated behind `ORPHEUS_LOADTEST=1` (plus the usual `ORPHEUS_TEST_*`
service env) so it never runs in the normal PR pipeline.

## Running

```bash
cd apps/api
export ORPHEUS_LOADTEST=1
export ORPHEUS_TEST_DATABASE_URL="postgres://orpheus_app:orpheus_app@localhost:5432/orpheus_test?sslmode=disable"
export ORPHEUS_TEST_NATS_URL="nats://localhost:4222"

# tunables (defaults shown)
export ORPHEUS_LOAD_DURATION=15s
export ORPHEUS_LOAD_CONCURRENCY=50
export ORPHEUS_LOAD_ARTIFACTS=200
export ORPHEUS_LOAD_MAX_ERROR_RATE=0.01   # fail over 1% 5xx/transport errors
export ORPHEUS_LOAD_MAX_P99_MS=1500

go test ./internal/e2e/ -run '^TestLoad_APIThroughput$' -v -count=1 -timeout 180s
```

## Baseline (local: in-process API, local Postgres/NATS, M-series)

Read/write mix, full-access API key, after the argon2id verification cache:

| concurrency | throughput | p50    | p99    | errors |
|-------------|------------|--------|--------|--------|
| 50          | ~3300 req/s| ~10ms  | ~130ms | 0      |
| 100         | ~2600 req/s| ~21ms  | ~296ms | 0      |
| 200         | ~2600 req/s| ~50ms  | ~390ms | 0      |

## Findings

1. **API-key argon2id was the wall (fixed).** Verification ran the
   CPU/memory-hard Argon2id on *every* request with no cache, capping the
   API at ~42 req/s and acting as a DoS vector. A short-TTL positive cache
   of successful verifications (revocation still checked via the fast
   indexed prefix lookup each request) took the same load from
   42 → ~1500 req/s at 30 concurrency (35×). See `internal/auth/apikey.go`.

2. **Next bottleneck: the DB connection pool.** Throughput plateaus and
   latency climbs past ~50 concurrent clients because the pgx pool is
   capped at `MaxConns=20` (`internal/db/db.go`). Raising it (and sizing
   Postgres accordingly) is the next lever; it is a deliberate conservative
   default today. Requests queue on the pool rather than erroring, so the
   system degrades gracefully (0 errors at 200 concurrency).

The load test asserts an error-rate and p99 ceiling so a gross regression
(e.g. re-introducing per-request argon2id) fails loudly.
