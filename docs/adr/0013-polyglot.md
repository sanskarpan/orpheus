# ADR-0013: Polyglot — Go API Tier + Python Worker Tier

- **Status:** Accepted
- **Date:** 2026-07-09
- **Deciders:** Principal Architect

## Context

The original design of Orpheus was Python-everywhere: a single FastAPI
codebase for both the public API and the worker plane (see ADR-0001,
ADR-0002 as originally written). That made sense as a starting
hypothesis: the team is small, one language is simpler to operate, and
Pydantic v2 + FastAPI is genuinely the most productive web stack in
Python.

A review surfaced that this is the **wrong choice for "designed for the
long term at scale"** in one specific place: the public API tier. The
API tier's workload is **high-RPS, low-logic, file coordination** — the
exact workload that is Go's sweet spot and Python's weakness:

- **High RPS, low logic.** The API tier is mostly: validate a JWT, look
  up a row, write a row, generate a presigned S3 URL, return JSON. There
  is no domain logic on the hot path. Every cycle spent in a Python
  framework is a cycle not spent serving a request.
- **File coordination.** Presigned URLs, multipart completion, range
  reads, content-type sniffing — these are I/O-bound and the API tier
  is the one place we touch bytes (introspection) or coordinate
  pointer-only hand-offs to S3.
- **No domain logic in Python.** None of the audio/ML code runs in the
  API tier. The API tier is the place where Python's productivity win
  is smallest.
- **The GIL is real here.** Under load, the API tier is where the GIL
  hurts most — many short requests, lots of socket reads and writes,
  no long-running compute to amortize it.
- **The cost math is real.** The same RPS target takes ~7× more pods
  on Python than on Go, ~10× more memory per pod, ~30× longer cold
  start, and ~15× larger container image. Annualized at 1M MAU, the
  delta is on the order of **$22k/yr** for the API tier alone — and
  it scales linearly with traffic.

The worker tier's situation is the opposite: every audio/ML library we
care about — `mutagen`, `librosa`, `ffmpeg` bindings, `faster-whisper`
(CTranslate2), `pyannote`, `demucs`, `musicgen`, `transformers`,
`onnxruntime` — is Python-first. There is no production Go ecosystem
for ASR, source separation, or generative audio. The worker tier stays
Python, full stop.

The public surface stays REST + OpenAPI (every client knows it; codegen
works in every language). The internal contract between the API tier
and the worker tier is the place where a strongly-typed schema and
low-latency RPC pay for themselves; that is gRPC + Protobuf.

## Decision

**Polyglot.** Orpheus is built as two tiers with two languages:

- **API tier (public HTTP).** **Go 1.22+** with the **Chi** router,
  `slog` for structured logging, `go.opentelemetry.io/otel` for
  traces, `pgx` + `sqlc` for PostgreSQL access, and `oapi-codegen` for
  OpenAPI generation from a hand-written `api.yaml`. Single static
  binary, distroless base, ~12 MiB image. The public REST surface is
  served entirely by this tier.
- **Worker tier (audio + ML).** **Python 3.12** with the existing
  audio/ML libraries (see §3.1 of the design doc for the full list).
  Includes **Arq** workers, **Temporal** workers, and **Ray Serve**
  GPU inference. A small **Python worker control plane** (FastAPI,
  per ADR-0002) exposes health, model-registry, and admin endpoints
  to the Go API tier over gRPC.
- **Inter-tier protocol.** **gRPC + Protobuf** for everything between
  the API tier and the worker tier. Schema lives in `packages/proto`;
  `buf` enforces lint and breaking-change detection in CI (per §3.13).

The public API contract (REST + OpenAPI 3.1, URI versioning, 6-month
sunset, webhooks, idempotency, RFC 7807) is unchanged. The change is
in the language the public API is **implemented** in.

## Consequences

### Positive

- **Cost.** ~$22k/yr cheaper at 1M MAU on the API tier alone, scaling
  with traffic. (See §3.1 of the design doc for the per-pod cost
  table.) The savings compound at 10M MAU and beyond.
- **Performance.** ~5–10× more RPS per CPU core on the API tier; ~10×
  smaller per-pod RSS; ~30× faster cold start; ~15× smaller image.
  This means fewer pods, faster HPA scale-out, faster Karpenter
  consolidation, and faster PR preview environments.
- **Operational.** No GIL means CPU/RPS is a meaningful HPA signal
  (no hidden serialization point). A static binary with no runtime
  tax is easier to debug, easier to supply-chain-harden, and easier
  to roll back. The image is small enough that we are nowhere near
  pulling-rate limits at ECR.
- **Worker tier unchanged.** The audio/ML ecosystem stays Python,
  where it belongs. We do not pay a tax on the side that actually
  needs the ecosystem.
- **Clear contract.** gRPC + Protobuf is a strongly-typed, fast,
  schema-evolvable boundary. `buf` catches breakage in CI. The
  public surface stays REST + OpenAPI for ergonomics.

### Negative

- **Two languages in the monorepo.** Two toolchains, two CI
  matrices (Go 1.22 + 1.23 and Python 3.12), two sets of
  dependencies, two sets of expertise to maintain. The monorepo
  pattern (ADR-0012) keeps the developer workflow uniform; the CI
  matrix doubles in size.
- **gRPC contract to maintain.** Protobuf schemas in
  `packages/proto` must be evolved carefully. `buf` mitigates this;
  it does not eliminate it.
- **Smaller hiring pool per tier.** "All Go" or "all Python" is
  easier to hire for than "Go for the API, Python for the workers".
  Mitigation: the API tier is small and idiomatic Go; the worker
  tier is standard ML/Python engineering. The total surface area
  per hire is smaller than for a full-stack Python role on an
  all-Python stack.
- **Local dev requires both runtimes.** The Tiltfile must build
  and run both the Go binary and the Python service. Hot-reload
  is good in both, but there are two processes to keep healthy.
  Mitigation: the Tiltfile is the single dev entrypoint.

### Neutral

- **Documentation language.** Public docs, API reference, and SDKs
  do not change. Internal architecture docs (§5, §6, §10) get
  updated to note the language per tier.
- **ADR-0002 (FastAPI).** Renamed and rescoped to the Python
  worker control plane only. The public API tier is no longer
  FastAPI; the worker control plane is.

## Alternatives Considered

- **Stay all-Python (status quo).** Rejected. The cost math in §3.1
  is real, the GIL is real, and the API tier's workload shape is
  Go's strongest case. "Python is more productive" is true for the
  worker tier (where we need the ML ecosystem) and not particularly
  true for the API tier (where there is no domain logic). We
  estimated the avoidable API-tier compute cost at 1M MAU at
  ~$22k/yr; over the lifetime of the system, that is the wrong
  trade.
- **All-Go.** Rejected. The worker tier cannot be Go. There is no
  production Go ecosystem for ASR (faster-whisper is CTranslate2
  with Python bindings), source separation (demucs is PyTorch),
  speaker diarization (pyannote is PyTorch), or generative audio
  (musicgen is Transformers). Wrapping every model in a
  Python-sidecar-via-gRPC is the tax we are explicitly trying to
  avoid, and it would re-introduce a Python dependency in the
  hot path of every job.
- **Rust for the API tier instead of Go.** Rejected. Rust would
  give us equal or better raw performance and a stronger type
  system. It would also give us a steeper learning curve, a
  smaller hiring pool, longer iteration cycles, and no real win
  for the I/O-bound HTTP workload the API tier actually does. Go
  is the right amount of performance for the right amount of
  effort. Rust remains on the table for the v2 streaming inference
  path (§17 Phase 8), where low-latency GPU work might benefit.
- **Polyglot with a different split (Python API, Go workers).**
  Rejected on the same grounds as "all-Python": the API tier is
  the wrong place to pay the GIL tax, and the workers have no
  reason to be Go.
- **Polyglot with Node/TypeScript on the API tier.** Rejected.
  Node has the same single-threaded-event-loop profile as Python
  under load; the cost/perf win is much smaller than with Go, and
  we would still be operating two languages.

## References

- `docs/architecture/PRODUCTION_DESIGN.md` §3.1 (Backend Language)
- `docs/architecture/PRODUCTION_DESIGN.md` §3.2 (API Framework, per
  tier)
- `docs/architecture/PRODUCTION_DESIGN.md` §5.1 (One-line description)
- `docs/architecture/PRODUCTION_DESIGN.md` §5.2 (Component map —
  the gRPC + Protobuf boundary is explicit in the ASCII diagram)
- ADR-0001 (Modular Monolith) — still valid in spirit; the two
  deployment units are now a Go API tier and a Python worker tier
  rather than a Python API and Python workers.
- ADR-0002 (FastAPI) — rescoped to the Python worker control plane
  in this revision.
- ADR-0009 (GitOps) — Go API image and Python worker image are
  built, signed, and tracked in the GitOps repo separately.
- ADR-0010 (Observability) — OpenTelemetry propagates trace
  context across the gRPC boundary.
- ADR-0012 (Monorepo) — `packages/proto` is the new shared
  artifact between the Go and Python sides.
- Replicate (Cog), Modal — production precedents for the
  Go/Rust-API + Python-workers split.
