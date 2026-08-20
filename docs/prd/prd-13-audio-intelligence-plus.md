# PRD: Audio-Intelligence Completion

**Status:** Proposed · **Priority:** P2 · **Epic:** Audio Intelligence · **Related issues:** #313 (PII redaction upgrade + audio beep), #368 (emotion recognition), #369 (audio-event detection), #323 (profanity / content moderation), #317-adjacent (auto-chapters as first-class output), #322 (multichannel / per-channel transcription), #320 (persistent speaker enrollment / voiceprints)

## 1. Summary

Orpheus has the transcript-analysis surface (sentiment, topics, entities,
summarize) and a regex-based PII redactor, but lacks the richer audio-intelligence
signals customers compare against the ASR leaders in
`docs/COMPETITIVE_ANALYSIS.md`. This PRD adds seven capabilities as new
processors registered through the existing `register_processor` catalog, plus
one Modal GPU service, reusing the established patterns (`text_ops.py` analysis
processors, `orpheus_diarize.py` GPU service, `redact.py` detector selection):

1. **PII redaction upgrade** (#313) — ML/LLM-grade text redaction beyond regex,
   plus **audio "beep" redaction** that mutes PII spans in the media.
2. **Emotion recognition** (#368) — per-segment emotion labels.
3. **Audio-event detection** (#369) — laughter, music, applause, etc.
4. **Profanity filter / content moderation** (#323) — flag/mask profanity and
   moderation categories.
5. **Auto-chapters as a first-class output** — promote `summarize`'s existing
   `chapters` mode into a structured `audio.chapters` processor.
6. **Multichannel / per-channel transcription** (#322) — transcribe each channel
   separately (e.g. call-center agent/customer).
7. **Persistent speaker enrollment / voiceprints** (#320) — recur speaker
   identities across jobs on top of ECAPA embeddings.

## 2. Motivation & goals

**Problem.** Redaction is regex-only in the default path (`redact.py:56`
`RegexDetector`, `DEFAULT_ENTITIES` at line 28) — it misses names/addresses
(Presidio exists at `redact.py:84` but only masks *text*, never the audio). There
is no emotion, no audio-event, and no profanity/moderation signal at all
(confirmed: no such code exists). Chapters are buried inside `text.summarize`
as a free-text `mode` (`llm.py:124`, `text_ops.py:142`) rather than a structured,
timestamped output. Transcription flattens audio to mono
(`convert_to_wav_16k_mono`, used at `processors/transcribe.py:72`), so stereo
agent/customer calls can't be separated. Diarization labels are anonymous S1..Sn
with **no** cross-job identity (`diarize.py:9`), so a returning speaker is never
recognized.

**Measurable goals.**
- ML/LLM redaction raises PERSON/LOCATION recall to ≥ 90% on a labeled set vs. the
  regex baseline's ~0% for those types; audio-beep muting covers 100% of masked
  text spans that have word timestamps.
- Emotion labels agree ≥ 70% with human labels on a curated emotion set.
- Audio-event detection F1 ≥ 0.75 for laughter/music/applause on a labeled clip set.
- Profanity flagging recall ≥ 95% on a profanity lexicon test; 0 false mutes on a
  clean control.
- Per-channel transcription keeps channels fully separated (0 cross-channel word
  bleed) on a synthetic 2-channel file.
- Enrolled speaker re-identification precision ≥ 90% at the chosen threshold on a
  held-out voiceprint set.

**Non-goals.** Realtime versions of these (prd-02 covers streaming); translation
of emotion/events; biometric identification of non-consenting individuals
(enrollment is explicit, consent-gated, opt-in).

## 3. Current state in Orpheus

- **Analysis processors** `apps/workers/src/orpheus_workers/processors/text_ops.py`:
  `text.sentiment` (line 226), `text.topics` (line 251), `text.entities`
  (line 280) — each resolves+redacts the transcript via `_load_text_for_analysis`
  (line 183), calls `get_llm()`, and parses JSON via `_analyze_json` (line 200).
  This is the template for any LLM-based analysis processor.
- **Redaction** `redact.py`: `RegexDetector` (line 56, structured PII + Luhn),
  `PresidioDetector` (line 84, ML NER, `pii` extra), `get_detector()` selection
  by `ORPHEUS_PII_ENGINE` (line 104), `redact_transcript` walks text/segments/words
  (line 147), `maybe_redact` hook (line 179). `text.redact` processor +
  pii_mapping artifact writing in `processors/redact.py`.
- **Diarization / ECAPA** `infra/modal/orpheus_diarize.py`: ECAPA embeddings
  (`_embed`, line 72), agglomerative clustering, VAD windowing (line 110). Worker
  side `diarize.py` `ModalDiarizer` (line 84), `get_diarizer()` (line 142),
  `manifest_identity()` (line 126). Result carries `segments[].speaker`
  (`audio_ops.py:162-174`).
- **Chapters (today)** `llm.py:124` `"chapters": "Break into titled chapters…"`;
  reachable only through `text.summarize` `mode="chapters"` (`text_ops.py:150`).
- **Audio conversion** `ffmpeg.py` `convert_to_wav_16k_mono` (mono downmix),
  `ffmpeg_slice` — both already used by transcribe/diarize processors.
- **Modal GPU pattern** `orpheus_diarize.py:47-56` (`@app.cls(gpu="a10g",
  min_containers=0, scaledown_window=300)`, `@modal.enter`, `@modal.method`,
  `@modal.fastapi_endpoint` + shared-secret) — the template for a new SenseVoice
  service.

## 4. Proposed design

### 4.1 SenseVoice Modal service (emotion + events) — #368, #369

- **New Modal GPU service** `infra/modal/orpheus_senses.py`, cloned from
  `orpheus_diarize.py`: CUDA image with `funasr`/SenseVoice (open, non-gated),
  ECAPA-style `@app.cls(gpu="a10g", min_containers=0, scaledown_window=300)`,
  `@modal.enter` loads SenseVoice-Small once, `@modal.method analyze(payload)` +
  `@modal.fastapi_endpoint(method="POST")` with the same
  `ORPHEUS_MODAL_SHARED_SECRET` token check (`orpheus_diarize.py:173-183`).
  Request `{token, audio_b64, segments?}`; response
  `{segments:[{start,end,emotion,events:[...],emotion_scores}], model_version_id,
  gpu_seconds}`. SenseVoice emits emotion + audio-event (SER + AED) in one pass,
  so #368 and #369 share the service.
- **Worker wiring.** New module `audio_intel.py` with `get_sense_analyzer()`
  mirroring `get_diarizer()`: `ModalSenseAnalyzer` when
  `ORPHEUS_SENSE_BACKEND=modal` + `ORPHEUS_MODAL_SENSE_URL`/`ORPHEUS_MODAL_SENSE_TOKEN`
  set, else a `StubSenseAnalyzer` (deterministic, for tests/CPU deploys), exactly
  like `StubDiarizer` (`diarize.py:32`).

### 4.2 Emotion processor (#368)

- `@register_processor("audio.emotion", tier="gpu_a10g", …)` in a new
  `processors/audio_intel.py`. Resolves the source audio + transcript
  (`_load_transcript`, `text_ops.py:34`), calls `get_sense_analyzer().analyze(wav,
  segments)`, and returns `{segments:[{start,end,text,emotion,emotion_scores}],
  dominant_emotion, model_version_id}`. Downmix via `convert_to_wav_16k_mono`
  as diarize does (`audio_ops.py:155`).

### 4.3 Audio-event detection (#369)

- `@register_processor("audio.events", …)` — same service call, returns
  `{events:[{start,end,label,confidence}], summary:{laughter:n, music:n, …}}`.
  Events are span-level (not tied to transcript segments) so music/applause during
  silence is still captured.

### 4.4 Profanity / content moderation (#323)

- **Two-tier**, selectable by `params.engine`: (a) **lexicon** — a
  dependency-free profanity word/phrase matcher over transcript text, reusing the
  span/offset masking machinery in `redact.py` (`redact_text`, line 125, with a new
  `PROFANITY` entity) so masked output uses the same `mask` modes
  (`type`/`char`/`hash`, `redact.py:117`); (b) **LLM moderation** — an
  `_analyze_json` call (`text_ops.py:200`) returning moderation categories
  (`{hate, harassment, sexual, violence, self_harm}` scores + flagged spans),
  identical in shape to the sentiment/topics processors.
- `@register_processor("text.moderate", …)` returns `{flagged: bool, categories,
  profanity:{count, masked_text?}, model_version_id}`. Opt-in masking via
  `params.mask_profanity`.

### 4.5 PII redaction upgrade + audio beep (#313)

- **Text upgrade.** Make ML/LLM detection first-class: keep `get_detector()`
  (`redact.py:104`) but add an `"llm"` engine that uses `get_llm().complete()` +
  `_analyze_json` to return PII spans for PERSON/ADDRESS/ORG (types regex can't
  catch), merged with `RegexDetector` structured hits. Selection stays
  `ORPHEUS_PII_ENGINE` (`regex`|`presidio`|`llm`).
- **Audio beep.** New `@register_processor("audio.redact", tier="cpu_medium", …)`:
  run `text.redact` to get PII spans, map each masked span to its word
  timestamps (requires `word_timestamps` — leans on prd-01 forced alignment for
  accuracy), then use `ffmpeg` to replace those time ranges in the media with a
  tone/silence (an ffmpeg `volume=0` / tone `aevalsrc` filter over the span list),
  emitting a redacted audio **artifact** (same artifact-writing pattern as
  `processors/redact.py:57` `_write_mapping_artifact`, with
  `sensitivity` flagging). Returns `{redacted_audio_artifact_id, redactions[]}`.

### 4.6 Auto-chapters as first-class output (#317-adjacent)

- Promote the existing `summarize(mode="chapters")` (`llm.py:124`) into
  `@register_processor("audio.chapters", …)` that returns **structured**,
  timestamped chapters: `{chapters:[{start, end, title, summary}]}`. Implementation
  maps LLM-proposed chapter boundaries back to segment timestamps (the LLM sees
  segments with times, returns titles + boundary segment indices via
  `_analyze_json`). Reuses `_load_transcript` and the untrusted-input system prompt
  (`text_ops.py:176` `_ANALYSIS_SYSTEM`). The old `summarize` `chapters` mode
  stays for back-compat (deprecation note only).

### 4.7 Multichannel / per-channel transcription (#322)

- Add `params.per_channel=true` to the transcribe processor path. Instead of
  `convert_to_wav_16k_mono` (`processors/transcribe.py:72`), split channels with
  ffmpeg (`pan`/`channelsplit`) into N mono 16 kHz wavs, transcribe each
  independently through the existing `transcribe()` engine, and label segments with
  `channel` (0,1,…). Result adds `segments[].channel` and a `channels:[{index,
  label}]` map; `params.channel_labels` lets a caller name them ("agent",
  "customer"). Reuses all existing chunking/offset logic per channel.

### 4.8 Persistent speaker enrollment / voiceprints (#320)

- **Enrollment.** `@register_processor("speaker.enroll", tier="gpu_a10g", …)`:
  takes a labeled audio sample, calls the ECAPA embed path (add an `/embed` method
  to `orpheus_diarize.py` returning the mean-normalized embedding, reusing `_embed`
  line 72), and stores `{speaker_id, org_id, name, embedding (vector)}` in a new
  tenant-scoped `speaker_profiles` table (RLS-scoped like all tenant data).
- **Recognition.** Extend `audio.diarize` (`audio_ops.py:132`) with an optional
  `params.identify=true`: after clustering into anonymous S1..Sn, compute each
  cluster's centroid embedding and match against enrolled profiles by cosine ≥
  `ORPHEUS_SPEAKER_MATCH_THRESHOLD`; relabel matched clusters with the enrolled
  `name` (fall back to S-labels for no-match). Returns
  `segments[].speaker_name?` alongside `speaker`.
- **Privacy.** Voiceprints are biometric data — enrollment is explicit and
  consent-gated; profiles honor GDPR erasure (`docs/prd/10-gdpr-erasure.md`), and
  embeddings are stored as a `sensitivity`-flagged resource. No enrollment happens
  implicitly from diarization.

### 4.9 Production hardening (all seven capabilities)

- **Error handling & failure modes / graceful degradation.** SenseVoice-backed
  processors (`audio.emotion`, `audio.events`) fall back to `StubSenseAnalyzer` when
  the Modal service is unreachable/timeout/non-200 and record a `warnings[]` entry —
  the job completes with empty/neutral labels rather than failing (as the acceptance
  bar in §6.5 requires). LLM-backed processors (`text.moderate`, `audio.chapters`,
  the `llm` PII engine) inherit the existing analysis-processor failure handling:
  malformed JSON is re-parsed/repaired via `_analyze_json`, and a hard LLM failure
  degrades — moderation falls back to the dependency-free lexicon engine, chapters
  fall back to a single whole-file chapter, and the `llm` PII engine falls back to
  merging only `RegexDetector` hits (never dropping the structured-PII guarantee).
  `audio.redact` requires word timestamps; when absent it falls back to
  segment-level muting (coarser, documented) rather than erroring, and if a masked
  span has no time mapping at all it is dropped from the audio filter but still
  reported in `redactions[]`. Multichannel falls back to mono if channel-split
  yields a single stream. Modal calls use bounded retry-with-jitter (max 2, no retry
  on 401), and repeated failure lands the job in the worker dead-letter path.
- **Scale, concurrency & bounded cost.** The SenseVoice Modal service mirrors
  `orpheus_diarize.py` (`@app.cls(gpu="a10g", min_containers=0,
  scaledown_window=300)`, `@modal.concurrent`) with a `max_containers` ceiling
  (`ORPHEUS_MODAL_SENSE_MAX_CONTAINERS`) and per-call audio-length cap so a long file
  is windowed rather than sent whole. LLM processors are cache-keyed by content
  (`cacheable` manifest field) so re-runs and identical inputs don't re-spend tokens;
  a per-job token/cost ceiling bounds moderation/chapters cost. `audio.redact`
  ffmpeg filter graphs are bounded by span count; `per_channel` transcription caps N
  at the real channel count. Worker-CPU processors respect existing concurrency
  limits; GPU work is gated so a CPU-only deploy runs every LLM/lexicon feature.
- **Multi-tenant security & RLS.** The new `speaker_profiles` table is `org_id`-RLS
  scoped exactly like all tenant data — every read/write in enrollment and
  `identify` is filtered by the job's `org_id`, and a voiceprint from one org can
  never match a cluster in another. Embeddings are stored `sensitivity`-flagged and
  honor GDPR erasure (`docs/prd/10-gdpr-erasure.md`). Audio sent to the SenseVoice /
  ECAPA-embed Modal endpoints is transient (never persisted; Volume holds weights
  only). Shared-secret token check is mandatory (`orpheus_diarize.py:173-183`).
  Redacted-audio and moderation artifacts inherit the artifact table's RLS. No
  implicit enrollment: voiceprints are created only by an explicit, consent-gated
  `speaker.enroll` call.
- **On-wire backward compatibility.** Every capability is a **new processor**
  (`audio.emotion`, `audio.events`, `text.moderate`, `audio.chapters`,
  `audio.redact`, `speaker.enroll`) or an **additive param/field** on an existing one
  (`params.per_channel` → `segments[].channel` + `channels[]`; `params.identify` →
  `segments[].speaker_name?`; `ORPHEUS_PII_ENGINE=llm`). The legacy
  `summarize(mode="chapters")` path stays for back-compat (deprecation note only).
  Existing result shapes for transcribe/diarize/redact are unchanged when the new
  params are omitted.
- **Observability.** Structured metrics per processor: `sense_gpu_seconds`,
  `sense_fallback_total{reason}`, `moderation_flagged_total{category}`,
  `pii_llm_spans_total{type}`, `audio_redact_spans_total`,
  `audio_redact_segment_fallback_total`, `per_channel_channels_total`,
  `speaker_identify_matches_total`, `speaker_enroll_total`. Fallbacks log once at
  WARN with job id + reason; every model-backed result carries `model_version_id`.
- **Cost metering.** SenseVoice/ECAPA-embed `gpu_seconds` metered through
  `ORPHEUS_WORKER_GPU_COST_USD_PER_SECOND`; LLM token cost metered through the
  existing analysis-processor cost path; `cost_per_job_usd` in each manifest reflects
  the true tier. Cached hits meter zero.
- **Config / env surface.** See §4.10.

### 4.10 Env / config surface

`ORPHEUS_SENSE_BACKEND`, `ORPHEUS_MODAL_SENSE_URL`, `ORPHEUS_MODAL_SENSE_TOKEN`
(parallel to the diarize trio, `diarize.py:121-122`);
`ORPHEUS_MODAL_SENSE_MAX_CONTAINERS`, `ORPHEUS_MODAL_SENSE_TIMEOUT_S`;
`ORPHEUS_SPEAKER_MATCH_THRESHOLD`; extend `ORPHEUS_PII_ENGINE` with `llm`. All read
once at worker start and surfaced in the startup config log; missing optional vars
fall back to CPU/stub defaults so a key-less deploy runs every non-GPU feature.

## 5. Delivery milestones

Milestones are **ordering only** — each is production-quality, hardened per §4.9,
and independently shippable, not a reduced-scope prototype. The full seven-capability
scope is committed; each milestone lands with its failure paths, RLS, metrics, cost
metering, and §6 acceptance bar met.

- **M1 — Auto-chapters (#317) + profanity/moderation (#323), production-complete.**
  Pure worker-CPU/LLM processors on the `text_ops.py` template with lexicon fallback
  for moderation, single-chapter fallback for chapters, caching, and metrics.
- **M2 — PII text upgrade (LLM engine) + `audio.redact` beep (#313),
  production-complete.** `llm` PII engine merged with regex (structured-PII
  guarantee preserved), audio beep with word-timestamp spans and segment-level
  fallback, sensitivity-flagged redacted-audio artifact.
- **M3 — SenseVoice Modal service → `audio.emotion` (#368) + `audio.events` (#369),
  production-complete.** GPU service with stub fallback, gpu-seconds metering,
  windowed long-file handling, `max_containers` ceiling.
- **M4 — Multichannel / per-channel transcription (#322), production-complete.**
  Additive params, per-channel chunking/offset reuse, mono fallback, 0 cross-channel
  bleed bar green.
- **M5 — Speaker enrollment / voiceprints (#320), production-complete.** RLS-scoped
  `speaker_profiles` table + migration, ECAPA `/embed`, `identify` flag, consent
  gating, and GDPR erasure shipped **with** the feature (highest sensitivity).

## 6. Verification / acceptance criteria

A senior QA runs these **end-to-end against a real worker + live Modal + a real
LLM backend** (not stubs, except where a failure path is deliberately induced).
Every numeric target is a hard gate; model-backed features are tested on both happy
and failure paths.

1. **Chapters (+ fallback):** submit a 40-min meeting; assert `audio.chapters`
   returns ≥ 3 chapters with monotonic non-overlapping `start/end` mapped to real
   segment times and non-empty titles. Force an LLM failure and assert a single
   whole-file chapter is returned (job completes, warning logged), not a 500.
2. **Moderation (+ fallback):** a clip with profanity → `text.moderate` flags it,
   `mask_profanity` masks tokens using the chosen `mask` mode; a clean clip returns
   `flagged:false` with 0 mutes (recall ≥ 95% on the lexicon test, 0 false mutes on
   the clean control). Force the LLM path down and assert degradation to the lexicon
   engine.
3. **PII upgrade:** a transcript naming a person + street address → `llm` engine
   detects PERSON/ADDRESS the regex engine misses (PERSON/LOCATION recall ≥ 90%);
   structured PII (email/CC) still caught via regex merge; Luhn CC check still holds;
   with the LLM down, structured-PII masking is still guaranteed.
4. **Audio beep (+ segment fallback):** run `audio.redact` on audio with a spoken
   phone number; assert the output artifact has the PII time span muted/beeped
   (100% of masked text spans that have word timestamps), text redactions match, and
   the artifact carries the sensitivity flag. Re-run without word timestamps and
   assert coarse segment-level muting fallback (documented), not an error.
5. **Emotion/events (+ stub fallback):** a clip with laughter + an angry utterance →
   `audio.events` labels laughter within the right span (F1 ≥ 0.75), `audio.emotion`
   labels the angry segment (≥ 70% human agreement); both record `gpu_seconds` and
   `model_version_id`; killing the Modal service mid-run degrades to the stub with a
   `warnings[]` entry and job success, not an error.
6. **Multichannel:** a synthetic stereo file (distinct speech per channel) with
   `per_channel:true` → segments carry `channel`, 0 cross-channel word bleed, labels
   applied from `channel_labels`; a mono file with `per_channel:true` falls back to
   single-channel cleanly.
7. **Enrollment + RLS + GDPR:** enroll speaker "Alice" from a consented sample; run
   `audio.diarize identify:true` on a new file where Alice speaks; assert her cluster
   is relabeled "Alice" (precision ≥ 90% at threshold), unknown speakers stay
   S-labeled; assert an org-B `identify` run never matches org-A's "Alice" profile
   (RLS isolation); a GDPR erasure removes the profile + embedding and a subsequent
   `identify` no longer matches.
8. **Concurrency / bounded cost:** drive concurrent `audio.emotion` jobs past the
   per-container cap; assert scale within `max_containers`, no dropped jobs, correct
   summed `gpu_seconds`; re-submit an identical LLM job and assert a cache hit meters
   zero new token cost.
9. **Back-compat:** the legacy `summarize(mode="chapters")` path still works;
   transcribe/diarize/redact results are unchanged when new params are omitted.

## 7. Dependencies, risks, open questions

- **Dependencies:** SenseVoice/FunASR on Modal + a new cache Volume; a
  `speaker_profiles` table + migration (tenant RLS); ffmpeg filter graphs for
  channel split + audio muting; `audio.redact` depends on prd-01 word timestamps
  for accurate spans; profanity lexicon data.
- **Risks:** (a) SenseVoice emotion/event taxonomies differ from customer
  expectations — expose raw scores, document the label set. (b) Audio-beep spans
  are only as good as word timestamps — require alignment; without it, fall back to
  segment-level muting (coarser). (c) Voiceprints are biometric/regulated —
  consent, retention, erasure must ship *with* the feature, not after. (d)
  LLM-based PII/moderation cost + latency — cache-key by content like existing
  analysis processors (`cacheable` manifest field).
- **Open questions:** vector storage for voiceprints — a `float[]` column vs.
  pgvector? Should `audio.emotion`/`audio.events` be one merged `audio.analyze`
  processor (single SenseVoice call) to save GPU seconds? Chapter granularity
  controls (target count vs. auto)?

## 8. Effort

**T-shirt: XL** (≈ 8–11 engineer-weeks; each milestone includes hardening, RLS,
metrics, and its acceptance bar — spans CPU processors, a new GPU service, schema,
and a regulated biometric feature).

- M1 (1 wk): auto-chapters + moderation/profanity + lexicon/single-chapter fallbacks.
- M2 (1.5 wk): PII LLM engine (regex-merge guarantee) + `audio.redact` beep + segment fallback.
- M3 (2 wk): `orpheus_senses.py` Modal service + emotion/events + stub fallback + metering.
- M4 (1 wk): multichannel transcription + mono fallback.
- M5 (2.5 wk): RLS `speaker_profiles` table + ECAPA embed + identify + consent + GDPR erasure.
