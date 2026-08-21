# PRD: Model & Deployment Differentiators

**Status:** Proposed · **Priority:** P2 · **Epic:** Models & Deployment · **Related issues:** #370 #371 #372 #373 #374 #375 #335

## 1. Summary

Turn Orpheus's model layer into a differentiator by making **which model runs where** a first-class, tenant-selectable decision. Deliverables: **on-device / edge model artifacts** packaged for whisper.cpp / WhisperKit / Moonshine / Parakeet (#371); **1000+ language coverage** via Meta **MMS** (#372); **speech-to-speech translation** via Soniox/Seamless (#370); a **human transcription tier** (#373); **custom model training / adaptation (BYO)** (#335); **forced alignment to an external reference text** via WhisperX / NeMo NFA (#374); and a **zero-data-retention / privacy-mode toggle** (#375).

All of it reuses two existing seams: the **S3-backed model registry** (`model_registry.py`, checksum-verified) as the source of truth for artifacts, and the **Modal service pattern** (`infra/modal/orpheus_transcribe.py` — shared-secret auth, scale-to-zero, weights on a Volume) as the GPU host. The new work is a **model-selection / routing story** (per-job / per-tenant model choice) plus artifact **packaging** for edge and new backends.

This PRD scopes the **complete production-grade** feature set — every capability ships tenant-ready, RLS-isolated, metered, observable, and fail-safe. Delivery is ordered into **Milestones (M1–M4)**; each milestone is itself a production-quality, shippable slice (not a reduced-scope prototype), and the full feature set above is in scope.

## 2. Motivation & goals

Orpheus's ASR core is currently a single whisper family (`tiny.en` CPU default, `large-v3-turbo` on Modal). Competitors differentiate on breadth (Deepgram/AssemblyAI language coverage, Seamless S2ST, on-device WhisperKit, human tiers). We already have the *plumbing* (registry, reproducibility pinning, Modal, provider-agnostic LLM). What's missing is (a) a way for a tenant to *choose* a model/backend per job, (b) artifacts for edge + non-whisper backends, and (c) a couple of new Modal services.

**Goals**
- A **model-selection API**: jobs can request a model/backend/language-strategy; the worker routes accordingly, pinning `(processor, version, model_version_id)` for reproducibility.
- **Edge artifacts**: signed, checksummed model bundles (whisper.cpp GGUF, CoreML/WhisperKit, Moonshine, Parakeet) downloadable for on-device inference; a thin edge SDK contract.
- New Modal services for **MMS** (#372) and **S2ST** (#370); a **forced-alignment** processor (#374).
- A **human transcription tier** (#373) as a job type with a human-in-the-loop state machine.
- **BYO / custom adaptation** (#335): register a tenant's fine-tuned/adapted model into a tenant-scoped registry namespace and route to it.
- **Privacy mode / ZDR** (#375): a per-tenant/per-job toggle that suppresses persistence and pins to in-memory / no-log backends.
- **Production-grade throughout:** every backend has defined failure modes with graceful degradation, bounded concurrency/cost, multi-tenant RLS isolation, additive on-wire shape, metrics/logs, and per-request cost metering.

**Non-goals**
- Building a training platform from scratch (#335 is *bring/adapt*, not full AutoML).
- Shipping the edge inference *app* — we ship signed artifacts + a contract; the app is downstream.

## 3. Current state in Orpheus (file:line, patterns to build on)

- **Model registry.** `apps/workers/src/orpheus_workers/model_registry.py` — `register()` uploads a weight file to S3 + records `(name, version, sha256, size)` in `model_registry` (`model_registry.py:48`); `resolve()` downloads to a local cache and **refuses to load on checksum mismatch** (`model_registry.py:95,119`). Migration `0018_model_registry.sql`. This is the artifact backbone for every new model.
- **Modal service pattern.** `infra/modal/orpheus_transcribe.py` — CUDA+cuDNN image, weights on a `modal.Volume` cache, `min_containers=0` scale-to-zero (`orpheus_transcribe.py:~50`), shared-secret auth via `X-Orpheus-Token` against `orpheus-modal-auth`. `infra/modal/orpheus_llm.py` (vLLM OpenAI-compatible) and `orpheus_diarize.py` prove the pattern generalizes.
- **Backend routing already exists — narrowly.** `transcribe.py:36` `ORPHEUS_WORKER_TRANSCRIBE_BACKEND` (`local`|`modal`), routed at `transcribe.py:127,181`; Modal URL/token from `ORPHEUS_MODAL_TRANSCRIBE_URL/TOKEN` (`transcribe.py:60`). We generalize this env-only switch into a **per-job** selection.
- **Reproducibility pinning.** Processor manifests pin `model_id`/`model_version_id` (`processors/__init__.py:` `ProcessorManifest`); `transcribe.py:22` sets `model_version_id=f"whisper-{...}"`. The content cache (`handlers/cache.go`) keys on model version — model choice must flow into this.
- **Provider-agnostic layer.** `llm.py` `get_llm()` shows the task-shaped abstraction we mirror for ASR backend selection.
- **Redaction / privacy primitives.** `processors/redact.py` (PII), `handlers/erasure.go`, retention sweeper (`internal/retention/sweeper.go`) — the levers privacy-mode (#375) toggles.
- **Word timestamps** already flow through `transcribe.py:54` and subtitle builders (`audio_ops.py:47`), the basis for alignment (#374).
- **Usage/metering rails.** Modal services return `gpu_seconds` (`orpheus_transcribe.py:142`); `usage/service.go` rolls per-job usage — every new backend meters through this unchanged.

## 4. Proposed design

### 4.1 Model-selection / routing (foundation for all of the below)
Add an optional `params.model` (or `params.asr`) to transcribe-family jobs. The field is **additive** — omitting it yields today's exact behaviour and today's cache key, so all existing clients and cached results are unaffected:
```jsonc
{ "processor": {"name":"orpheus.transcribe","version":"2.0.0"},
  "params": { "model": {
      "backend": "whisper|mms|seamless|byo",
      "name": "large-v3-turbo",        // or "mms-1b-all", "seamless-m4t-v2", or a BYO name
      "language_strategy": "auto|force|mms-lid",
      "target_language": "es"          // for S2ST/translate
  }}}
```
- A worker-side **`select_model()`** resolver (generalizing `_backend()`, `transcribe.py:36`) maps the request to `(backend, model_version_id, endpoint)`, resolving weights through `model_registry.resolve()` (checksum-verified) or a Modal endpoint.
- The resolved `model_version_id` is written back into the result and the manifest pin so the **content cache** (`cache.go`) and reproducibility stay correct — different model ⇒ different cache key.
- **Validation & failure modes.** Only registry-known models (or tenant-registered BYO scoped to the caller's org) are accepted, mirroring `validation.py` param-guarding (`transcribe.py:13`); unknown/unauthorized names are rejected at job-create with a 4xx and a stable error code (never a silent fallback to the default model, which would corrupt reproducibility). If a *selected* backend is transiently unavailable (Modal cold-start timeout, 5xx), the resolver applies a **bounded retry with jitter**, then either (a) fails the job to the dead-letter path with a `backend_unavailable` reason, or (b) — only when the tenant sets `params.model.fallback: "default"` — degrades to the platform default and records the substitution in the result + audit so it is never silent.
- **Scale & concurrency.** `select_model()` enforces per-tenant concurrency ceilings and a global per-backend inflight cap so one tenant/one exotic backend cannot starve the default path; over-cap jobs queue with backpressure (NATS) rather than fanning out unbounded Modal containers.
- **Observability & cost.** Emit `model_selected{backend,name,model_version_id,org_id}` metrics/logs per job; carry `gpu_seconds` and backend into `usage/service.go` so each backend is independently cost-metered and billable.
- Available models are surfaced via `GET /v1/processors/{name}` and `GET /v1/models` (extend the manifest to list supported backends) so clients can discover choices.

### 4.2 On-device / edge artifacts (#371)
- **Packaging.** Extend `model_registry` with a `format`/`target` dimension (`gguf`, `coreml`, `onnx`, `ct2`) and a `variant` (quantization). A `package.edge` build job converts a base model into the edge format, uploads via `model_registry.register()` (so every edge blob is checksummed). Build jobs run bounded (one conversion per model/format at a time) and are idempotent on `(name, version, format, variant)`.
- **Distribution.** `GET /v1/models` and `GET /v1/models/{name}/{version}/download` return a **signed, expiring URL** to the checksummed artifact (reuse the `artifacts.go` presign pattern, `artifacts.go:133`) plus the sha256 the client must verify — the on-device story inherits the registry's tamper-refusal (`model_registry.py:119`).
- **Security.** Download endpoints are auth-scoped (`auth/principal.go`); presign TTLs are short; platform-global artifacts are read-public/service-write, BYO edge artifacts are org-scoped under RLS (§4.7). Every issued download URL is audited (`audit`) for supply-chain traceability.
- **Failure modes.** Missing/half-uploaded artifact ⇒ 404 with a clear code, never a URL to a partial blob; checksum mismatch on the client is the client's hard-stop, mirroring server tamper-refusal.
- **Backends:** whisper.cpp (GGUF), WhisperKit (CoreML, Apple), Moonshine + Parakeet (ONNX/NeMo export). No server inference change — these are downloadable artifacts + a documented client contract (same `{text,segments,language}` shape).

### 4.3 1000+ languages via Meta MMS (#372)
- New Modal service `infra/modal/orpheus_mms.py` (mirror `orpheus_transcribe.py`: CUDA image, weights on a Volume, shared secret, scale-to-zero) serving MMS ASR + MMS-LID (language id) for 1000+ languages. Weights registered in `model_registry`.
- Selected via `backend: "mms"`; `language_strategy: "mms-lid"` runs LID first. Returns the standard transcript shape so diarize/subtitles/translate compose unchanged.
- **Production concerns.** Bounded max audio duration + chunking to cap per-job GPU cost; input-language guardrails (reject/ flag unsupported requests rather than silently mis-transcribing); returns `gpu_seconds` for metering; structured logs on LID confidence. On service error, job dead-letters with `backend_unavailable` (or optional whisper fallback when the language is whisper-covered and the tenant opted in).

### 4.4 Speech-to-speech translation (#370)
- New processor `speech.translate` + Modal service `infra/modal/orpheus_s2st.py` hosting **Seamless (SeamlessM4T v2)** for S2ST/S2TT; Soniox as an optional external provider behind the same selection interface.
- Input audio + `target_language` → `{translated_text, segments, translated_audio_artifact?}`. Translated audio stored as a derived artifact (delivered via signed URL / bundle). Composes with `prd-06` dubbing (#367).
- **Production concerns.** `target_language` validated against a supported-pairs table; derived audio artifacts are org-scoped (RLS) and honor the tenant's retention/ZDR policy. External-provider path (Soniox) is **blocked for ZDR jobs** (§4.8) and only reachable when the tenant permits third-party processing. GPU cost metered per second; long inputs chunked with backpressure.

### 4.5 Forced alignment to external reference text (#374)
- New processor `align.forced` — inputs `{artifact_id, reference_text}`; aligns known text to audio (WhisperX or NeMo Forced Aligner) producing word/segment timestamps. Runs on the transcribe Modal service (add an aligner entrypoint) or CPU for short clips. Output plugs straight into `export.subtitles` (`audio_ops.py:178`) and the `prd-06` text-driven editor. Distinct from transcription: text is given, only timing is inferred.
- **Production concerns.** `reference_text` size-bounded; on gross text/audio mismatch (alignment score below threshold) the processor returns a `low_confidence` flag rather than fabricating timings, so downstream consumers can gate. CPU vs GPU routing chosen by clip length to bound cost.

### 4.6 Human transcription tier (#373)
- Job type `transcribe.human` with a longer SLA and a **review state machine** (`queued_for_human → in_review → completed`, plus `rejected`/`expired` terminal states for SLA breach), backed by a `human_review_tasks` table (org-scoped, **FORCE RLS**) and a reviewer console (internal or vendor API). Typically seeded by a machine transcript, then corrected. Priced higher; metered through the existing usage rails. Result shape identical, with a `review_meta` block.
- **Production concerns.** State transitions are transactional and audited; SLA timers drive `expired` → automatic fallback to the machine transcript so a stalled human tier never hangs a job forever. Reviewer access to a tenant's audio is RLS-scoped and consent/retention-honoring; PII exposure to human reviewers is gated by the tenant's redaction policy. Vendor API calls are bounded/retried and treated as external processing (blocked under ZDR).

### 4.7 Custom model / adaptation (BYO) (#335)
- **Tenant-scoped registry namespace.** Allow `model_registry` rows scoped to an org (add `org_id`, **FORCE RLS** on tenant rows; global catalog stays read-public/service-write as today, `model_registry.py:8`). A tenant registers a fine-tuned CT2/whisper checkpoint or a domain adaptation (custom vocab/biasing) via `POST /v1/models` (upload → checksum → register).
- Selection via `backend:"byo", name:"<tenant-model>"`; the resolver restricts BYO names to the caller's org (RLS enforces it). Lightweight adaptation first (custom vocabulary / phrase biasing on top of whisper), full fine-tune import second.
- **Production concerns.** Uploads are size/format-validated and checksummed before registration; a BYO artifact is **quarantined** (loaded in an isolated Modal container, never co-resident with another tenant's weights) and smoke-tested on a canary clip before it is routable. Per-org storage quotas bound cost. Tenant attests rights to the weights (audited). RLS test is a release gate: org A's BYO model must be invisible and unselectable to org B.

### 4.8 Privacy mode / zero-data-retention (#375)
- Per-tenant + per-job `privacy: "zdr"` toggle. When set: no transcript/segment persistence to the `prd-12` store, no result body in logs, `retain_until = now()` (immediate sweep) or in-memory-only return, and routing **pinned to no-log backends** (own Modal services, never an external provider that logs). Enforced centrally in job create (`jobs.go`) + result write, and audited (`audit`). Complements PII redaction (`redact.py`) and erasure (`erasure.go`).
- **Fail-closed guarantee.** If a ZDR job would require a backend that cannot honor no-log/no-retain (e.g. an external S2ST provider, a vendor human tier), the job is **rejected at create**, not silently downgraded. Audio buffers for ZDR jobs are transient and zeroed after processing. Log lines for ZDR jobs carry only IDs and metrics, never content — verified by a redaction test on the log pipeline.

### 4.9 Where it runs
- All new GPU model backends: **Modal**, one app per model family, shared-secret auth, scale-to-zero, weights on Volumes and pinned in `model_registry`. Each app exposes health + `gpu_seconds` and has bounded concurrency and warm-pool options for popular models.
- Edge artifacts: built server-side, distributed as signed checksummed downloads; inference on the client.
- Selection/routing + human-tier state machine + BYO registration: Go API + worker resolver.

### 4.10 Discovery & selection example
```jsonc
GET /v1/models            // catalog: platform models + this org's BYO models
// → { "models": [
//      { "name": "large-v3-turbo", "backend": "whisper", "formats": ["ct2","gguf","coreml"],
//        "languages": "multilingual", "version": "2.0.0", "model_version_id": "whisper-large-v3-turbo" },
//      { "name": "mms-1b-all", "backend": "mms", "languages": "1000+", "version": "1.0.0" },
//      { "name": "acme-legal-v3", "backend": "byo", "org_scoped": true } ] }

GET /v1/models/large-v3-turbo/2.0.0/download?format=gguf
// → { "url": "https://s3...signed...", "sha256": "e3b0c4...", "expires_at": "..." }
```

### 4.11 User stories
- As an on-device app builder, I want a signed, checksummed WhisperKit artifact so my iOS app transcribes offline with a model I trust (#371).
- As an NGO, I want to transcribe a low-resource language whisper doesn't cover, via MMS (#372).
- As a localization team, I want speech-in-Spanish → speech-in-English in one call (#370).
- As a bank, I want a ZDR job where nothing is stored or logged and no external provider ever sees the audio, and where such a job is *rejected* rather than downgraded if it can't be honored (#375).
- As a medical-scribe vendor, I want to upload my fine-tuned model and route my jobs to it, invisible to and unselectable by other tenants (#335).

## 5. Rollout / milestones

Each milestone is independently production-quality and shippable — full multi-tenant, metered, observable, fail-safe. Milestones order the work; none is a reduced-scope prototype.

- **M1 — Selection foundation (production).** `params.model` + worker `select_model()` resolver with validation, bounded retry/fallback, per-tenant/per-backend concurrency caps, metrics, and cost metering + `GET /v1/models` discovery. Additive on-wire shape; cache-key correctness proven. Unblocks everything else.
- **M2 — Breadth (production).** MMS service (#372) and Seamless S2ST (#370) as new Modal apps + processors, each with duration/cost bounds, language guardrails, retention/ZDR honoring, metering, and dead-letter behavior.
- **M3 — Alignment + edge (production).** `align.forced` (#374) with confidence gating and edge packaging/distribution (#371) with signed/audited downloads, org-scoped BYO edge artifacts, and idempotent bounded build jobs.
- **M4 — Tiers & control (production).** Human tier (#373) with a transactional/audited state machine + SLA fallback, BYO/adaptation (#335) with quarantine + canary + RLS gating, privacy/ZDR mode (#375) with fail-closed enforcement.

## 6. Verification / acceptance criteria

End-to-end against a **real worker + Go API + Modal** (not unit-only), with negative/failure paths and numeric targets. Multi-tenant isolation checks are release gates.

- **Selection & compatibility.** A job requesting `backend:"mms"` transcribes a non-whisper-covered language on the live MMS Modal service and returns the standard `{text,segments,language}` shape; an unknown/unauthorized model name is rejected at job-create with a stable 4xx error code and **no** fallback. A request with no `params.model` produces byte-identical output and the same cache key as pre-change (additive-shape proof).
- **Cache correctness.** Same audio with two different models yields two distinct cached results, and re-running each hits the cache (no cross-contamination in `cache.go`); measured cache-hit on repeat = 100%.
- **Integrity.** Every model (server or edge) is loaded/served only after sha256 verification; a deliberately tampered blob is refused server-side (`model_registry.py:119`) and the edge `download` response's sha256 matches the registry checksum for the artifact. A partial/missing artifact returns 404, never a URL.
- **Resilience.** With the selected Modal backend forced to 5xx/timeout, the job either dead-letters with `backend_unavailable` or (only with `fallback:"default"`) completes on the default model with the substitution recorded in result + audit — never silently. Under a burst exceeding the per-backend inflight cap, excess jobs queue (backpressure) and the default path stays within its latency SLO.
- **Alignment.** `align.forced` on audio + correct reference text produces word timestamps within a stated tolerance (e.g. median |Δ| ≤ 100 ms) of a ground-truth alignment and feeds `export.subtitles` unchanged; on grossly wrong text it returns `low_confidence` rather than fabricated timings.
- **S2ST.** `speech.translate` returns translated text (and an audio artifact when requested) for a sample pair on the live Seamless service; the derived artifact honors the tenant's retention policy.
- **Privacy / ZDR.** A `privacy:"zdr"` job leaves no transcript row in the `prd-12` store, no result body in logs (verified by scanning the log pipeline output), immediate `retain_until`, and never routes to a logging external provider; a ZDR job that *would* require a logging/retaining backend is **rejected at create** (fail-closed), audited.
- **Multi-tenant isolation.** A BYO model registered by org A is not listed in, and not selectable by, org B (RLS test); org B's attempt returns the same "unknown model" rejection as a nonexistent name. BYO weights never co-reside with another tenant's in a container (quarantine assertion). Human-tier reviewer access to org A's audio is not reachable from org B.
- **Metering.** Each backend's `gpu_seconds` and backend label reach `usage/service.go`; a scripted mixed-backend run produces a per-backend, per-org usage rollup that reconciles with the emitted metrics.

## 7. Dependencies, risks, open questions

- **Depends on** `prd-12` (privacy mode toggles its persistence/retention; residency interacts with per-region model deployment) and composes with `prd-06` (S2ST → dubbing, alignment → text editor).
- **Cost / cold starts.** Each model family is another scale-to-zero Modal app; cold-start latency and GPU cost multiply — reuse Volume caches, apply per-backend inflight caps, and use warm pools for popular models. Cost is bounded per-job by duration/chunk limits and metered per-org.
- **Licensing.** MMS/Seamless model licenses and Soniox terms must permit hosted commercial use; BYO import needs a tenant attestation of rights (audited).
- **Registry scope change.** Adding `org_id` to `model_registry` must preserve the current global-catalog read-public/service-write semantics for platform models while FORCE-RLS-isolating tenant rows.
- **Human tier ops.** Reviewer sourcing/quality/turnaround is an operational build, not just code; SLA-breach fallback to machine transcript bounds the failure blast radius.
- **Open:** ONNX vs CoreML vs GGUF priority for edge? Seamless self-host vs Soniox API default for S2ST (note Soniox is external-processing, ZDR-blocked)? How is a BYO fine-tune sandboxed/canaried before it is routable?

## 8. Effort

- M1 selection/routing + discovery (production-hardened): ~2.5 wk.
- M2 MMS + S2ST Modal services + processors: ~4–5 wk.
- M3 forced alignment + edge packaging/distribution: ~3–4 wk.
- M4 human tier + BYO + privacy mode: ~5–7 wk (human tier ops is the long pole).

Order-of-magnitude: **~3.5–4.5 months** total; M1 (the routing seam every other item needs) is ~2.5 weeks and low-risk.
