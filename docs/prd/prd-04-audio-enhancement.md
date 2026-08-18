# PRD: Audio Enhancement (Krisp-class)

**Status:** Proposed · **Priority:** P1 · **Epic:** Audio Enhancement · **Related issues:** #345, #346, #347, #348, #349, #350

## 1. Summary

Add a Krisp-class audio-enhancement capability to Orpheus: a family of DSP + neural
front-ends that clean audio before (or instead of) transcription. The complete scope
is a single `audio.enhance` processor with pluggable **modes** — AI noise suppression
(#345), background-voice cancellation (#346), echo cancellation / de-reverberation
(#347), voice isolation (#348), telephony 8 kHz denoise (#350), and accent conversion
(#349) — every mode production-grade. Heavy neural inference runs on **Modal GPU/CPU**
services behind the same shared-secret HTTPS pattern already used by
`orpheus-transcribe` and `orpheus-diarize`; a lightweight RNNoise path runs in-worker
for cheap/low-latency jobs. A realtime path taps the streaming relay so live sessions
get denoised audio before ASR under a strict added-latency budget.

## 2. Motivation & goals

Orpheus today transcribes whatever audio it is handed. Real-world audio (call-center 8 kHz,
noisy field recordings, reverberant rooms, cross-talk) degrades WER on every downstream
processor: `transcribe`, `audio.diarize`, `text.*`. Cleaning the signal is the single highest-
leverage quality lever and is a standalone product surface (Krisp, Adobe Enhance, ElevenLabs
Isolator) customers already pay for.

Goals:
- One versioned, cacheable `audio.enhance` processor that emits a cleaned WAV artifact plus
  quality metrics, composable ahead of `transcribe`/`audio.diarize` via `source_job_id`/`artifact_id`.
- Open, non-gated models only (DeepFilterNet, RNNoise, Demucs, an SEP/echo model), mirroring the
  "real model without a gated checkpoint" choices already made for diarization (SpeechBrain ECAPA)
  and LLM (Qwen2.5, non-gated).
- Modal GPU/CPU services with scale-to-zero, shared-secret auth, same env-wiring shape as existing
  Modal apps.
- Optional realtime enhancement in the streaming path with a strict added-latency budget.

Non-goals: music mastering/EQ, loudness normalization beyond what ffmpeg already does,
speaker anonymization/voice conversion beyond the accent-convert mode, and a client UI
(these are platform primitives).

## 3. Current state in Orpheus   (file:line, patterns to build on)

- **Processor registry + manifest.** `apps/workers/src/orpheus_workers/processors/__init__.py:55`
  (`register_processor`) with `ProcessorManifest` fields including `tier` (enum
  `cpu_tiny…gpu_a100`, `__init__.py:30`), `cacheable`, `cost_per_job_usd`, `model_id`,
  `model_version_id`, `slo_p95_seconds`. Catalog is synced to DB at startup (`__init__.py:6`).
- **A processor that downloads an artifact, converts to WAV, runs a model, uploads a new artifact,
  and inserts an `artifacts` row** already exists end-to-end: `audio.diarize` +
  `export.subtitles` in `processors/audio_ops.py:122` and `:178`, with the artifact-insert helper
  at `audio_ops.py:233` (`_insert_artifact`). This is the exact shape `audio.enhance` reuses.
- **ffmpeg helpers:** `convert_to_wav_16k_mono` and `slice` in `orpheus_workers/ffmpeg.py`
  (imported at `transcribe.py:10`, `audio_ops.py:23`).
- **Modal offload pattern.** `infra/modal/orpheus_transcribe.py` — `@app.cls` + `@modal.enter`
  (`:56`) warming weights on a `modal.Volume` (`:41`), `@modal.fastapi_endpoint(method="POST")`
  (`:147`) with shared-secret check against `ORPHEUS_MODAL_SHARED_SECRET` (`:157`), `gpu="a10g"`,
  `min_containers=0`, `scaledown_window=300` (`:50`). `orpheus_diarize.py` shows the CPU/DSP-heavy
  variant on `debian_slim` with a non-gated model.
- **Worker→Modal client** with `local`/`modal` backend switch and base64 audio payload:
  `transcribe.py:34` (`_backend`), `transcribe.py:39` (`_transcribe_modal`) reading
  `ORPHEUS_MODAL_TRANSCRIBE_URL` / `_TOKEN` (`:56`). `diarize.py:121` mirrors this with
  `ORPHEUS_MODAL_DIARIZE_URL/_TOKEN`.
- **Streaming engine** `streaming.py` — `StreamSession.add_audio` (`:164`) buffers PCM16 mono and
  decodes via an injectable `Transcriber` callable (`:48`), with energy VAD at
  `streaming.py:234`. The Go relay `streaming_ws.go:114` pumps browser↔worker frames and meters
  PCM bytes (`:184`). A realtime enhance hook slots in front of `add_audio`.

## 4. Proposed design   (architecture, models/algorithms, new processors/endpoints/schema, API shapes, where it runs)

### 4.1 New processor: `audio.enhance`

Register in a new `processors/audio_enhance.py` following `audio_ops.py:122`:

```python
@register_processor(
    "audio.enhance",
    display_name="Audio Enhancement",
    description="Denoise / isolate voice / de-reverb / echo-cancel an audio artifact.",
    tier="gpu_a10g",          # cpu_medium for the RNNoise-only fast path
    timeout_seconds=1800,
    cost_per_job_usd=0.02,
    model_id=_ENHANCE_MODEL_ID,          # from enhance.manifest_identity()
    model_version_id=_ENHANCE_MODEL_VERSION,
    input_schema={"mode": "str", "artifact_id|source_job_id": "ref", "sample_rate": "int?"},
    output_schema={"artifact_id": "str", "metrics": "obj"},
)
```

Behaviour: resolve source artifact (reuse the resolution logic in `audio_ops.py:36`), download,
`convert_to_wav_16k_mono` (or 48 kHz for isolation modes), dispatch to the selected engine, upload
the cleaned WAV, insert the artifact via `_insert_artifact` (`audio_ops.py:233`), return
`{artifact_id, s3_key, mode, metrics, model_version_id}`. `metrics` carries input/output RMS,
estimated SNR gain, and `gpu_seconds` for cost metering (same field the Modal transcribe endpoint
returns, `orpheus_transcribe.py:142`).

**Modes → models** (all Apache/MIT/permissive, non-gated):

| Mode (issue) | Model / algorithm | Where it runs |
|---|---|---|
| `denoise` (#345) | DeepFilterNet3 (full-band, low-latency); RNNoise fast path | Modal GPU (DFN) / in-worker CPU (RNNoise) |
| `background_voices` (#346) | Demucs / MelBand-Roformer source separation, keep dominant speaker | Modal GPU |
| `echo_dereverb` (#347) | WPE dereverb + a small AEC net (SpeexDSP AEC for the linear path) | Modal GPU |
| `voice_isolation` (#348) | Demucs vocals stem or a speech-enhancement SE model, 48 kHz | Modal GPU |
| `telephony_denoise` (#350) | DeepFilterNet tuned for 8 kHz narrowband + upsample to 16 kHz | Modal GPU / CPU |
| `accent_convert` (#349) | open accent-conversion model (e.g. an any-to-many VC net) | Modal GPU |

`mode` selects the engine; a single job runs one mode. Chaining (denoise → dereverb) is done the
existing way — one job per step linked by `source_job_id`, exactly as PRD 04's
transcribe→translate→summarize chain.

### 4.2 New Modal service: `orpheus-enhance`

New file `infra/modal/orpheus_enhance.py`, structurally identical to `orpheus_transcribe.py`:
- `@app.cls(gpu="a10g", volumes={CACHE_DIR: model_cache}, secrets=[auth], min_containers=0,
  scaledown_window=300)` with `@modal.enter` loading DFN/Demucs weights onto a
  `modal.Volume.from_name("orpheus-enhance-cache")`.
- `@modal.fastapi_endpoint(method="POST")` taking `{token, audio_b64, mode, sample_rate,
  params}` → `{audio_b64, sample_rate, mode, metrics, gpu_seconds}`; token checked against
  `ORPHEUS_MODAL_SHARED_SECRET` (reuse the `orpheus-modal-auth` Secret, as `orpheus_llm.py:39`
  already does).
- Base image: CUDA+cuDNN registry image (as `orpheus_transcribe.py:34`) since DFN/Demucs use
  torch on GPU; ffmpeg apt-installed for resampling.

Worker client `orpheus_workers/enhance.py` mirrors `transcribe.py:39`: `_backend()` reads
`ORPHEUS_ENHANCE_BACKEND` (`local`|`modal`), `_enhance_modal()` posts base64 audio to
`ORPHEUS_MODAL_ENHANCE_URL` with `ORPHEUS_MODAL_ENHANCE_TOKEN`, and a `local` RNNoise/DSP path for
key-less/test runs. `manifest_identity()` returns `(model_id, model_version_id)` so the manifest
advertises what will actually run (pattern: `llm.py:293`, `diarize.py`).

### 4.3 Realtime path (behind a flag, latency-gated)

The streaming engine decodes PCM16 mono via the injectable `Transcriber` (`streaming.py:48`).
Add an optional `enhancer: Callable[[bytes, int], bytes]` to `StreamConfig`/`StreamSession`
applied inside `add_audio` (`streaming.py:164`) before buffering, so LocalAgreement-2 and the VAD
operate on cleaned audio. For realtime we require a frame-synchronous, low-latency model
(DeepFilterNet or RNNoise run in-process in the worker), **not** a Modal round-trip. A `start`
control frame field `enhance: "denoise"` (parsed at `streaming.py:299`) toggles it; the Go relay
needs no change since it is transport-agnostic (`streaming_ws.go:169`). Billing already meters PCM
bytes server-side (`streaming_ws.go:196`); enhancement adds no new metering surface.

### 4.4 API shapes

No new HTTP endpoints — `audio.enhance` is a processor on the existing `POST /v1/jobs`
(`server.go:210`) and is discoverable via `GET /v1/processors/{name}` (`server.go:220`).

```jsonc
{ "artifact_id": "<raw audio artifact>",
  "processor": { "name": "audio.enhance", "version": "1.0.0" },
  "params": { "mode": "denoise", "strength": 0.9 } }
```

Result (in `jobs.result`): `{ "artifact_id": "...", "s3_key": "enhanced/<org>/<job>.wav",
"mode": "denoise", "metrics": { "snr_gain_db": 11.4, "gpu_seconds": 2.1 }, "model_version_id":
"deepfilternet3:..." }`.

### 4.5 Production hardening (all modes)

- **Error handling & failure modes / graceful degradation.** When
  `ORPHEUS_ENHANCE_BACKEND=modal` and the `orpheus-enhance` endpoint is
  unreachable/timeout (`ORPHEUS_MODAL_ENHANCE_TIMEOUT_S`)/non-200, GPU modes with a
  CPU equivalent (`denoise`, `telephony_denoise`) fall back to the in-worker
  RNNoise/DSP path and record a `warnings[]` entry; GPU-only modes
  (`background_voices`, `voice_isolation`, `echo_dereverb`, `accent_convert`) fail
  the job cleanly with a typed error rather than silently returning un-enhanced
  audio labeled as enhanced. Bounded retry-with-jitter (max 2, no retry on 401)
  mirrors `_transcribe_modal`. A model that produces degenerate output (output RMS
  ≈ 0, or `snr_gain_db` below a floor) is rejected and the original artifact is
  passed through with a warning rather than shipping destroyed audio. Repeated
  failure lands the job in the worker dead-letter path. In the realtime path, an
  enhancer exception or per-chunk over-budget disables enhancement for the rest of
  the session (stream continues on raw audio) instead of dropping the stream.
- **Scale, concurrency & bounded cost.** `orpheus-enhance` mirrors the transcribe
  service (`@app.cls(gpu="a10g", min_containers=0, scaledown_window=300)`,
  `@modal.concurrent`) with a `max_containers` ceiling
  (`ORPHEUS_MODAL_ENHANCE_MAX_CONTAINERS`) so heavy Demucs/isolation bursts can't
  fan out unboundedly. Per-call audio length is capped and long files are windowed;
  a per-job `timeout_seconds` (1800) and `gpu_seconds` budget bound cost. The
  content-hash dedup cache makes `audio.enhance` deterministic per
  `(artifact, mode, params, model_version)` so re-runs cost nothing. Heavy modes are
  GPU-only and non-realtime by policy; the RNNoise path stays CPU and cheap.
- **GPU/CPU limits.** RNNoise `denoise` runs `tier=cpu_medium` so a key-less/CPU-only
  deploy has a fully working enhancement product; every GPU mode is gated behind the
  Modal backend flag and degrades or errors explicitly when GPU is unavailable.
- **Multi-tenant security & RLS.** Enhanced-audio artifacts are written through
  `_insert_artifact` (`audio_ops.py:233`) into the `org_id`-RLS-scoped `artifacts`
  table under an `enhanced/<org>/<job>.wav` key; no cross-tenant read/write. Audio
  posted to `orpheus-enhance` is transient (base64 in-request, never persisted;
  Volume holds weights only), token-checked against `ORPHEUS_MODAL_SHARED_SECRET`.
  A per-org data-egress gate (`allow_external_llm`-style) governs whether audio may
  leave for Modal at all.
- **On-wire backward compatibility.** `audio.enhance` is a **new** processor on the
  existing `POST /v1/jobs`; no existing result shape changes. Its output composes
  ahead of `transcribe`/`audio.diarize` via `source_job_id`/`artifact_id` exactly
  like existing chains, so nothing downstream needs to know it ran. The streaming
  `start.enhance` field defaults off and the Go relay is unmodified.
- **Observability.** Metrics: `enhance_gpu_seconds`, `enhance_snr_gain_db`
  (histogram), `enhance_fallback_total{mode,reason}`,
  `enhance_degenerate_rejected_total`, `enhance_realtime_added_ms` (histogram),
  `enhance_realtime_disabled_total`. Fallbacks/rejections log once at WARN with job
  id + mode + reason; every result carries `model_version_id`.
- **Cost metering.** `gpu_seconds` from the enhance endpoint is metered through the
  existing GPU-cost path (same field the transcribe endpoint returns,
  `orpheus_transcribe.py:142`); `cost_per_job_usd` per mode reflects the real tier;
  CPU RNNoise and cache hits meter zero GPU.
- **Config / env surface.** `ORPHEUS_ENHANCE_BACKEND` (`local`|`modal`),
  `ORPHEUS_MODAL_ENHANCE_URL`, `ORPHEUS_MODAL_ENHANCE_TOKEN`,
  `ORPHEUS_MODAL_ENHANCE_TIMEOUT_S`, `ORPHEUS_MODAL_ENHANCE_MAX_CONTAINERS`, plus a
  realtime added-latency budget knob. Read once at worker start, surfaced in the
  startup config log; missing optional vars fall back to the CPU RNNoise path.

## 5. Delivery milestones

Milestones are **ordering only** — each is production-quality, hardened per §4.5,
and independently shippable, not a reduced-scope prototype. The complete mode set
(including realtime and accent conversion) is committed scope; each milestone lands
with its fallback behavior, metrics, cost metering, RLS, and §6 acceptance bar met.

1. **M1 — CPU denoise, production-complete.** `audio.enhance` with `mode=denoise`
   on the in-worker RNNoise path (`tier=cpu_medium`), no Modal dependency: processor,
   artifact output, metrics, degenerate-output rejection, cache dedup.
2. **M2 — Modal GPU service + DFN modes, production-complete.** Deploy
   `orpheus-enhance`; wire `ORPHEUS_ENHANCE_BACKEND=modal`; DeepFilterNet `denoise` +
   `telephony_denoise` (#345, #350) with 401 auth, retry/timeout, RNNoise fallback,
   `gpu_seconds` metering, `max_containers` ceiling.
3. **M3 — Separation modes, production-complete.** `voice_isolation` (#348) and
   `background_voices` (#346) via Demucs; `echo_dereverb` (#347) via WPE+AEC —
   GPU-only, windowed for long files, explicit-error on GPU loss.
4. **M4 — Realtime enhancement, production-complete.** Streaming enhancer hook +
   `start.enhance` flag, latency-gated to DFN/RNNoise with the < 50 ms budget
   enforced and auto-disable-on-overbudget.
5. **M5 — Accent conversion, production-complete.** `accent_convert` (#349) as a
   flagged GPU mode with the same fallback/metering/RLS guarantees.

## 6. Verification / acceptance criteria   (concrete e2e tests)

A senior QA runs these **end-to-end against a real worker + a live
`orpheus-enhance` Modal deployment** (not stub-only, except where a failure path is
deliberately induced). Every numeric target is a hard gate; each mode is tested on
its happy path and its failure path.

- **DSP correctness (pure).** DSP/wrapper functions are pure and injectable like the
  subtitle builders (`audio_ops.py:89`): feed a synthetic noisy WAV (tone + white
  noise), assert output RMS-in-noise-band drops and `metrics.snr_gain_db > 0`.
- **Processor e2e (real GPU).** Submit `POST /v1/jobs` with `mode=denoise` to the
  live Modal backend, poll to `completed`, assert a new `artifacts` row exists,
  `content_type=audio/wav`, `result.gpu_seconds` is metered and billed, and
  `result.model_version_id` matches `enhance.manifest_identity()`.
- **Modal failure fallback.** Stop `orpheus-enhance` mid-run with `mode=denoise` and
  assert graceful fallback to the in-worker RNNoise path (`warnings[]` set, job
  completes); with `mode=voice_isolation` (GPU-only) assert a clean typed error, not
  silent pass-through of un-enhanced audio. Inject a 401 and assert no retry storm.
- **Degenerate-output guard.** Feed audio that drives a mode to near-silent output;
  assert the original artifact is passed through with a warning and
  `enhance_degenerate_rejected_total` increments — destroyed audio never ships as
  "enhanced".
- **Chain e2e.** `audio.enhance` → `transcribe` via `source_job_id`; assert the
  second job reads the enhanced artifact and completes.
- **Quality regression.** On a fixed noisy golden set, WER of
  `transcribe(enhance(x))` ≤ WER of `transcribe(x)` across every shipped mode
  (tracked like the diarization/lang-detect golden sets referenced in PRD 04/05).
- **Modal contract test.** Endpoint rejects a missing/bad `token` with 401 (assert
  against `orpheus_transcribe.py:159`) and returns the documented shape incl.
  `gpu_seconds`.
- **Concurrency / cost ceiling.** Drive concurrent enhance jobs past the
  per-container cap; assert scale within `max_containers`, no dropped jobs, correct
  summed `gpu_seconds`; re-submit an identical `(artifact, mode, params, version)`
  and assert a cache hit meters zero.
- **Multi-tenant isolation.** Two orgs submit concurrently; assert each enhanced
  artifact is written only under its own `org_id` key/RLS and no audio persists on
  the enhance Volume after the run; assert the per-org egress gate blocks Modal
  offload when disabled.
- **Realtime latency + degradation.** With the streaming enhancer enabled, assert
  added per-chunk latency < 50 ms on CPU for `min_chunk_seconds=1.0` audio and
  partial/final ordering unchanged; force an enhancer error mid-stream and assert
  enhancement auto-disables while the stream continues on raw audio
  (`enhance_realtime_disabled_total` increments).

## 7. Dependencies, risks, open questions

- **Dependencies:** `orpheus-modal-auth` Secret (exists), Modal GPU quota, a new
  `orpheus-enhance-cache` Volume, ffmpeg (already in worker image), torch on Modal.
- **Cost/latency risk:** Demucs/isolation are heavy; keep them GPU-only and non-realtime. Cache
  aggressively — `audio.enhance` is deterministic per `(artifact, mode, params, model_version)` so
  it plugs straight into the content-hash dedup cache (PRD 01).
- **Model licensing:** verify each checkpoint is non-gated/permissive (the explicit constraint that
  drove SpeechBrain ECAPA in `orpheus_diarize.py` and Qwen2.5 in `orpheus_llm.py`).
- **Realtime AEC** needs the far-end reference signal; browser echo cancellation may already run
  client-side, so server AEC is best-effort for non-WebRTC ingest (ties into PRD 05 telephony).
- **Open questions:** Do we normalize output loudness? Do isolation modes emit multiple stems as
  separate artifacts (like `export.subtitles` emits multiple formats, `audio_ops.py:211`)? Per-org
  data-egress policy for audio sent to Modal — reuse the `allow_external_llm`-style gate.

## 8. Effort

Each milestone below includes its fallback behavior, metrics, cost metering, RLS,
and acceptance bar — not just the happy path.

- M1: Processor `audio.enhance` + worker `enhance.py` client + local RNNoise path +
  degenerate guard + cache: **~1.5 wk**.
- M2: Modal `orpheus-enhance` service (DFN + telephony) incl. deploy/warm/volume +
  auth/retry/timeout + RNNoise fallback + metering: **~1.5 wk**.
- M3: Separation modes (Demucs/WPE/AEC), GPU-only + windowing: **~1.5 wk**.
- M4: Realtime streaming hook + flag + latency budget + auto-disable + tests: **~1 wk**.
- M5: Accent conversion mode + fallback/metering/RLS: **~1 wk**.
- Full production scope (all modes + realtime): **~6–7 wk**.
