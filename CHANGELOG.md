# Changelog

All notable changes to Orpheus are documented here. This project adheres to
[Semantic Versioning](https://semver.org/).

## [0.1.0] — 2026-08-11

First tagged stable release. This is a verified baseline cut after a full
pre-release stabilization audit (regression re-audit + full-system E2E sweep +
all quality gates green). Every item below was verified by driving the live
API/worker/GPU, not by code inspection alone.

### Added

- **GPU transcription via Modal.** A deployable Modal service runs faster-whisper
  **large-v3-turbo** (`float16`) on an A10 GPU behind an authenticated HTTPS
  endpoint, with a Volume-cached model and scale-to-zero. The worker offloads
  transcription to it when `ORPHEUS_WORKER_TRANSCRIBE_BACKEND=modal` (default
  stays local CPU). See `infra/modal/README.md`.
- **Configurable ASR core.** Model, device, and `compute_type` are env/param
  driven (default **int8** on CPU); language **auto-detects** (previously
  hard-coded to English); custom vocabulary / keyterm biasing; per-request model
  selection; optional model warmup.
- **Real cost metering for GPU.** GPU jobs are billed on reported `gpu_seconds`
  at a configurable GPU rate (default ≈ A10 $1.10/hr); CPU jobs bill wall-clock.
- **Server-side streaming metering.** The relay meters received PCM16 audio and
  bills the session on that, not a client-reported duration.
- **Uniform list envelope.** Every list endpoint returns
  `{data, has_more, next_cursor}`, and empty lists return `[]` (never `null`).
- **Competitive analysis & backlog.** `docs/COMPETITIVE_ANALYSIS.md` and
  `FEATURES-AND-ISSUES.md` catalog the market landscape and the full
  issue/feature backlog.

### Fixed

- **Content-cache collision (critical, wrong results).** Upload-complete stored
  `sha256=''` and the job cache keyed on it, so different audio could return
  another input's cached transcript. Now the real content hash is computed and
  stored at upload, and the cache is skipped for any blank hash.
- **Budget hard-cap PATCH truncation.** `NULLIF($2,0)` inferred the limit as an
  integer, silently dropping fractional updates (`10.99→10`, `0.000001→0`). Cast
  to `float8` so cents and sub-dollar caps persist and enforce.
- **`slice` dead-lettered on uploaded artifacts.** Extension-less S3 keys
  produced a `.bin` output that `ffmpeg -c copy` couldn't mux. The output
  extension is now derived from `content_type`.
- **Honest processor manifests.** `audio.diarize` no longer advertises
  `pyannote` while running the round-robin stub, and `text.summarize`/
  `text.translate` no longer claim `orpheus-llm` while echoing stubs; the catalog
  (and cache key) reflect the actual configured engine.
- **Streaming WebSocket close.** The relay sends a normal `1000` close frame
  instead of leaving peers with an abnormal `1006`.
- **Empty lists returned `null`.** All 13 list handlers now return `[]`.
- Plus earlier stabilization fixes to the SaaS auth/onboarding flow (redirect
  loop, API-key prefix collision, webhook delivery listing).

### Verified behaviors (stabilization audit)

- ASR on both backends: Modal GPU (`large-v3-turbo`) and local CPU (`int8`),
  with correct multilingual auto-detection (en/de/es), per-request model, and
  custom vocabulary.
- Content cache: distinct audio → distinct transcripts; byte-identical input →
  correct cache hit.
- GPU-seconds billing exactness; Modal endpoint rejects unauthenticated calls.
- Streaming bills server-metered duration (a spoofed client value is ignored).
- Budget hard-cap returns HTTP 402 when exceeded and allows when under.
- All 13 processors execute end-to-end (transcribe, convert-to-wav, probe,
  extract-metadata, slice, diarize, detect-language, summarize, translate,
  redact, export-subtitles, export-bundle; URL-ingest guarded by SSRF checks).
- Multi-tenant RLS isolation (cross-org reads 404); API-key auth (401 on
  missing/invalid); rate limiting; input-validation error paths (400/404/
  dead-letter).
- Quality gates green: `go test`/`go vet`/`gofmt`, `pytest` (3.12 & 3.13),
  `ruff` check+format, `pyright`, buf, OpenAPI validation, E2E pipeline.

### Known limitations

- **Audio intelligence is stubbed by default.** Diarization is a round-robin
  stub unless `ORPHEUS_DIARIZE_MODEL` (pyannote) is configured; summarize/
  translate/detect-language use a deterministic stub unless `ANTHROPIC_API_KEY`
  is set. The manifests now report this honestly.
- **GPU transcription requires Modal configuration** (deploy `infra/modal` +
  `ORPHEUS_WORKER_TRANSCRIBE_BACKEND=modal` + endpoint/token). The default is
  local CPU (`tiny.en`), which is slower and English-only.
- **Webhook delivery** requires a public HTTPS endpoint; the API correctly
  rejects non-HTTPS/localhost URLs (SSRF protection), so live delivery was not
  exercised in the local audit (validation was).
- Streaming still re-transcribes a sliding window (higher latency/cost at
  scale); realtime diarization, VAD endpointing, and inference batching are
  tracked as follow-ups.

[0.1.0]: https://github.com/sanskarpan/orpheus/releases/tag/v0.1.0
