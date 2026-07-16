# Design 10 — GPU inference plane, model registry, and the `diarize` processor

> **Status:** proposed · **Owner:** ML platform / workers · **Scope:** gap #10
> **Depends on:** [ADR-0005 (model versioning)](../adr/0005-model-versioning.md), [ADR-0008 (gVisor)](../adr/0008-gvisor-sandbox.md), [PRODUCTION_DESIGN §3.10, §5.2, §10.2](../architecture/PRODUCTION_DESIGN.md)
> **Companion TODO:** [`apps/workers/DIARIZE_TODO.md`](../../apps/workers/DIARIZE_TODO.md) (processor contract; TODO doc only — no code under `apps/workers/src`).

Design + explicit build checklist. No code is added by this design under
`apps/workers/src/`; the processor contract lives in the companion TODO.

---

## 1. Problem

Today whisper runs **in-process** inside the CPU worker: `transcribe.py` lazily
loads a `faster-whisper` `WhisperModel` on first use, from a directory pointed
to by `ORPHEUS_WORKER_WHISPER_DIR`. That works for a `tiny.en` model on CPU. It
does **not** scale to real GPU inference, and it does not support the next
processors the product needs (`diarize`, `demucs`, `musicgen`):

| Gap | Today | Consequence |
|---|---|---|
| GPU serving | in-process, one model, one process | no batching, no fractional GPU, no autoscale; a GPU pod runs one job at a time |
| Model distribution | a local dir, env-var path | no provenance, no checksum, no reproducibility; "which weights ran?" is unanswerable |
| Model catalog | none | ADR-0005's `model_version_id` reproducibility contract is not backed by anything |
| Isolation | workers process untrusted audio in the host kernel | libavcodec/pyannote CVE class can escape (ADR-0008) |
| Diarization | not implemented | can't answer "who spoke when"; blocks `diarize+align` workflow |

This design introduces: a **GPU worker pool served by Ray Serve**, an
**S3-backed model registry with checksum verification**, the **gVisor sandbox**
for the processing plane, and the **`diarize` processor** (pyannote) plus
alignment onto the whisper transcript.

---

## 2. Proposed architecture

```
        Temporal / NATS activity: transcribe_chunk, diarize, ...
                       │  (calls the GPU plane, does not host models)
                       ▼
   ┌───────────────────────────────────────────────────────────┐
   │ Ray Serve (GPU inference plane) — one control plane        │
   │                                                            │
   │  Deployment: whisper-large-v3   @A10G  (dynamic batching)  │
   │  Deployment: pyannote-3.1       @A10G  (diarization)       │
   │  Deployment: demucs-htdemucs    @A10G×2                    │
   │  Deployment: ecapa/panns        @CPU   (embeddings/tag)    │
   │                                                            │
   │  Each deployment: fractional-GPU aware, autoscaling on     │
   │  queue depth + latency, per-tenant fairness via a routing  │
   │  policy, request timeout + GPU OOM guard.                  │
   └──────────────┬────────────────────────────────────────────┘
                  │ pulls weights at replica init
                  ▼
   ┌───────────────────────────────────────────────────────────┐
   │ Model registry:  Postgres (catalog)  +  S3 (weight blobs)  │
   │  model_versions(sha256, s3_uri, license, params_schema...) │
   │  loader verifies sha256 BEFORE handing weights to Ray      │
   └───────────────────────────────────────────────────────────┘

   All processing pods (Arq, Temporal activities, Ray replicas) run under
   runtimeClassName: gvisor — seccomp, no egress except VPC endpoints,
   CPU/mem/time/FD limits (ADR-0008).
```

### 2.1 GPU worker pool (Ray Serve)

- **Why Ray Serve, not Triton** (ADR / §3.10): heterogeneous models, low batch,
  Python-native, fractional GPU, single control plane. Triton is premature for
  our low-QPS heterogeneous mix.
- **One Ray Serve app** hosts multiple deployments; each deployment is a model
  family pinned to a hardware class (A10G for whisper/pyannote/demucs, CPU for
  small embedding/tagging models — matches §10.2).
- **Dynamic batching** on whisper (many small chunks arrive from the
  `transcribe-long` fan-out). Diarization is not batched (one recording at a
  time, longer).
- **Autoscaling:** Ray Serve replica autoscaler on `num_ongoing_requests` +
  latency; node autoscale via Karpenter on the GPU node pool (spot 60–80%,
  on-demand fallback — §10.9).
- **Per-tenant fairness:** a routing/admission layer in front of Ray enforces
  the per-tenant GPU bulkhead (§12) so one tenant's fan-out can't starve others.
  Concretely: a Redis token bucket per `(tenant, model_family)` gates admission
  before the request reaches a GPU replica.
- **Failure modes:** GPU OOM → deployment returns a retriable error, the
  Temporal activity retries on a fresh replica; replica health check evicts a
  wedged CUDA context.

### 2.2 S3 model registry (checksum-verified)

The `model_versions` table already exists in the schema (§9.1). This design
gives it teeth:

- Each `model_version` row records: `model_id`, semver, `s3_uri` of the weight
  bundle (a tarball or a directory prefix), **`sha256`** of the bundle, `format`
  (`ctranslate2` | `pyannote` | `torch` | `onnx`), `params_schema` (JSON Schema
  for processor params), `license`, `deprecated_at`.
- **Publishing** a model version is an offline, audited operation: upload bundle
  to S3 (SSE-KMS), compute sha256, insert the row. Immutable thereafter.
- **Loading** (in a Ray replica or worker at init):
  1. Read `model_version` row (catalog).
  2. Download bundle from `s3_uri` to node-local scratch (gp3, encrypted).
  3. **Recompute sha256 and compare to the catalog value. Refuse to load on
     mismatch** (integrity + supply-chain defense).
  4. Cache on the node keyed by `sha256` so replicas on the same node share it.
- **Reproducibility (ADR-0005):** every `Result` records the exact
  `model_version_id`. Same input + same `model_version_id` ⇒ same output,
  forever, because the weights are content-addressed by sha256.
- **Migration path from today:** the current `ORPHEUS_WORKER_WHISPER_DIR` becomes
  one seeded `model_versions` row (`whisper` family, `ctranslate2` format,
  sha256 of the local bundle uploaded to S3). `transcribe.py`'s loader is
  refactored to go through the registry loader instead of a raw env path.

### 2.3 gVisor sandbox

- All processing pods set `runtimeClassName: gvisor` (ADR-0008). This is the
  actual defense for our threat: `ffmpeg`/`libavcodec`/`pyannote` decode
  attacker-controlled bytes; gVisor intercepts syscalls in userspace so a
  kernel-memory-safety exploit in a decoder cannot escape to the host kernel.
- Seccomp profile + **no network egress** (models come from S3 via a VPC
  endpoint on an allow-listed path, not the open internet), CPU/mem/time/FD
  limits, read-only rootfs, non-root UID.
- ~5–15% perf overhead, accepted (ADR-0008). GPU pods use gVisor with the
  nvidia-container-runtime shim; the design validates GPU passthrough under
  gVisor as a build task (it is the one non-trivial integration).

### 2.4 The `diarize` processor (pyannote) + alignment

- **`diarize`** runs `pyannote/speaker-diarization-3.1` on the source audio and
  returns speaker-labeled time segments: `[{start, end, speaker}]` plus
  `num_speakers`.
- **Alignment** is a separate, CPU-cheap step that merges diarization segments
  with a whisper transcript: for each transcript segment, assign the speaker
  whose diarization interval maximally overlaps it, producing
  `[{start, end, text, speaker, confidence}]` — exactly the enriched segment
  shape in `PRODUCTION_DESIGN §7.2`.
- **Orchestration:** `diarize+align` is a **Temporal workflow** (§5.4 of the
  main design), two dependent activities: `diarize` (GPU) then `align` (CPU),
  where `align` also consumes the transcript from a prior/parallel `transcribe`.
  This slots directly into the design #9 Temporal plane.
- **Contract:** the full processor contract (inputs, outputs, params schema,
  errors, resource envelope, model version) is specified in
  `apps/workers/DIARIZE_TODO.md`. **No processor code is added by this design.**

---

## 3. Data-model changes

Additive, in a new migration `0006_model_registry.sql` (**author under
`apps/api/internal/db/migrations/`; out of scope for this doc — do not touch
existing migrations**). If `model_versions` already exists from `0001`, this
migration only *adds columns*:

```sql
ALTER TABLE model_versions
  ADD COLUMN IF NOT EXISTS s3_uri        text,
  ADD COLUMN IF NOT EXISTS sha256        text,      -- content address; verified on load
  ADD COLUMN IF NOT EXISTS format        text,      -- ctranslate2|pyannote|torch|onnx
  ADD COLUMN IF NOT EXISTS params_schema jsonb,     -- JSON Schema for processor params
  ADD COLUMN IF NOT EXISTS license       text,
  ADD COLUMN IF NOT EXISTS deprecated_at timestamptz,
  ADD CONSTRAINT model_versions_sha256_len CHECK (sha256 IS NULL OR length(sha256) = 64);

-- pyannote gated models need a HF token stored as a secret, not in the DB.
```

No new tenant table (model catalog is global, read-only to tenants via
`GET /v1/processors`). Results already carry `model_version_id` (§7.2).

---

## 4. API surface

Public (mostly read; publishing is internal/admin):

```
GET  /v1/processors                    # includes 'diarize', 'transcribe', ...
GET  /v1/processors/diarize            # params schema, SLO, cost, versions
GET  /v1/processors/diarize/versions   # model_version list (sha256, license)
POST /v1/jobs   {processor:"orpheus.audio.diarize", version:"3.1.x", ...}
POST /v1/workflows/diarize-align  {artifact_id, params}   # NEW workflow
```

Internal:

```
# model registry admin (not tenant-facing)
POST /internal/models/{id}/versions    # publish: s3_uri + sha256 + license
# Ray Serve is called by activities over the in-cluster service address;
# admission gated by the per-tenant token bucket.
```

---

## 5. Rollout plan

1. **Registry first, no behavior change.** Migrate `whisper` to a
   `model_versions` row; refactor `transcribe.py`'s loader to the checksum-
   verifying registry loader. Verify identical transcripts (byte-for-byte on the
   same input). This de-risks the registry independently of GPU.
2. **Stand up Ray Serve** with a single `whisper-large-v3` deployment on one
   GPU node pool. Route `transcribe_chunk` activities to Ray behind a flag;
   shadow-compare against in-process whisper. Validate gVisor + GPU passthrough
   here.
3. **Add `pyannote-3.1` deployment.** Implement `diarize` processor per
   `DIARIZE_TODO.md`. Ship `diarize` as a standalone job first (no alignment).
4. **Add `align`** (CPU) + the `diarize+align` Temporal workflow (design #9).
5. **Per-tenant bulkhead + autoscaling tuning** under load test (k6/§14).
6. **Extend** to `demucs`, then `musicgen`, reusing the registry + Ray + gVisor
   pattern.

Rollback per step = flag off (route back to in-process whisper; Ray deployments
scale to zero).

---

## 6. Risks

| Risk | Likelihood | Mitigation |
|---|---|---|
| GPU passthrough under gVisor is finicky | Med–High | Isolated spike in step 2 before any product traffic; fall back to runc for GPU pods **only** with compensating controls (strict seccomp + no-egress) if gVisor+GPU blocks, documented as a deviation from ADR-0008. |
| pyannote weights are gated (HF token) | High | Store HF token in Secrets Manager; mirror the accepted weights into our S3 registry (license permitting) so runtime pulls never hit HF. |
| Model bundle sha256 mismatch on load stalls a pool | Med | Fail fast + alert; publishing is immutable + verified at publish time, so mismatch means corruption/tampering — correct to refuse. |
| GPU cost blowout | Med | Spot 60–80%; scale-to-zero on idle deployments; per-tenant bulkhead; cost recorded per job (§7.2) with the $/MAU alert (ADR-0011). |
| Diarization accuracy / speaker over-splitting | Med | Expose `num_speakers` hint param; ship quality eval set; version pin so tenants can pin a known-good model_version. |
| Ray Serve control-plane single point of failure | Med | HA Ray head; activities retry on a fresh replica; NATS/Temporal make the job durable regardless. |
| Alignment mis-assigns speakers on overlapped speech | Med | Overlap-max heuristic + confidence output; document as best-effort; overlap-aware diarization is a v2 model upgrade. |

---

## 7. What must be built (checklist)

- [ ] Ray Serve app + GPU node pool (Karpenter provisioner, spot mix) with a
      `whisper-large-v3` deployment (dynamic batching) and a `pyannote-3.1`
      deployment.
- [ ] Model registry loader: catalog read → S3 download → **sha256 verify** →
      node-local cache keyed by sha256 → hand to Ray/processor. Refuse on
      mismatch.
- [ ] Migration `0006_model_registry.sql` (additive columns on
      `model_versions`: `s3_uri`, `sha256`, `format`, `params_schema`,
      `license`, `deprecated_at`). **Do not edit existing migrations.**
- [ ] Model publish tooling (`POST /internal/models/{id}/versions`): upload
      bundle, compute sha256, insert immutable row; audited.
- [ ] Refactor `transcribe.py` loader to go through the registry (seed whisper
      as `model_versions` row #1); verify identical output.
- [ ] Per-tenant GPU admission bulkhead (Redis token bucket per
      `(tenant, model_family)`).
- [ ] `diarize` processor implementing the contract in
      `apps/workers/DIARIZE_TODO.md` (pyannote-3.1 via Ray).
- [ ] `align` processor (CPU): overlap-max speaker assignment onto transcript
      segments → enriched `[{start,end,text,speaker,confidence}]`.
- [ ] `diarize+align` Temporal workflow (ties into design #9).
- [ ] gVisor runtimeClass on all processing pods; **validate GPU passthrough
      under gVisor**; seccomp profile; no-egress network policy; VPC endpoint
      for S3 model pulls.
- [ ] HF token in Secrets Manager; mirror pyannote weights into the S3 registry.
- [ ] Cost recording for GPU seconds per `diarize`/`transcribe` job (§7.2);
      wire the ADR-0011 $/MAU alert.
- [ ] Quality eval set + version-pinning docs for diarization.
- [ ] Load test (k6) for the fan-out + diarization path; autoscaler tuning.
