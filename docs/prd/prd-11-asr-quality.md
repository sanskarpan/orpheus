# PRD: ASR Quality Completion

**Status:** Proposed · **Priority:** P1 · **Epic:** ASR Core Quality · **Related issues:** #298 (forced alignment), #299 (ITN / smart formatting), #395 (VAD-segmented batch chunking), #321 (code-switching / multi-language)

## 1. Summary

Orpheus transcription is functionally complete but its output quality trails the
ASR leaders it is benchmarked against in `docs/COMPETITIVE_ANALYSIS.md` (the
"strong platform, weak ASR core" gap). This PRD closes four quality gaps that
share one code path — `apps/workers/src/orpheus_workers/processors/transcribe.py`
and the underlying `transcribe.py` engine — without changing the on-wire result
shape (`{text, segments, language, duration_seconds}`, optionally `segments[].words`):

1. **Forced-aligned word timestamps** (#298) — replace Whisper's cross-attention/DTW
   word times with wav2vec2 forced alignment (WhisperX-style) for frame-accurate
   boundaries used by subtitles (`export.subtitles`) and streaming.
2. **Inverse text normalization (ITN) + smart formatting** (#299) — spoken-form →
   written-form ("twenty twenty six" → "2026", "$5", "3:30 pm"), plus truecasing
   and punctuation, as an opt-in post-processing pass.
3. **VAD-segmented long-file chunking** (#395) — replace the fixed 60 s window
   splitter with speech-boundary-aware segmentation so words are never cut
   mid-utterance and silence is skipped.
4. **Code-switching / multi-language per utterance** (#321) — detect and label
   language at the segment level so a single call handles mixed-language audio.

## 2. Motivation & goals

**Problem.** `processors/transcribe.py:85-99` splits long audio on arithmetic
60-second boundaries (`n_chunks = int(duration // chunk_seconds) + …`), which
can slice a word or sentence in half at every boundary, degrading WER and
producing broken segment text at the seams. Word timestamps come straight from
faster-whisper (`transcribe.py:203-213`, `w.start/w.end` from cross-attention),
which drift 100–300 ms and hurt caption sync. Output is raw spoken form — no
digits, currency, or casing normalization — so transcripts read poorly and
fail downstream formatting expectations. Language is detected once for the whole
file (`transcribe.py:219`, `info.language`) and forced globally
(`processors/transcribe.py:92`, `language = topts["language"] or "en"`), so
bilingual audio is mis-recognized.

**Measurable goals.**
- Word-timestamp median absolute boundary error ≤ 50 ms on a 30-utterance golden
  set (vs. Whisper-DTW baseline measured first).
- No word split across chunk boundaries on files > chunk length (0 mid-word cuts
  in the acceptance corpus); WER on a 60-min golden file improves ≥ 1.0 absolute
  point vs. fixed-window chunking.
- ITN pass converts ≥ 95% of numbers/dates/currency/times in a labeled set to
  correct written form with 0 regressions on already-correct text.
- Per-segment language labels correct on ≥ 90% of segments in a curated
  code-switch (en↔es, en↔hi) corpus.

**Non-goals.** New realtime/streaming alignment (covered by prd-02); changing the
default whisper model; translation (already `text.translate`); custom-vocabulary
biasing (already implemented, `transcribe.py:133-151`).

## 3. Current state in Orpheus

- **Transcription engine** `apps/workers/src/orpheus_workers/transcribe.py`:
  faster-whisper with a per-tuple model cache (`_models`, line 27), a `local`
  vs `modal` backend switch (`_backend()`, line 34), and `_transcribe_modal()`
  (line 39) posting base64 wav to `ORPHEUS_MODAL_TRANSCRIBE_URL`. Word timestamps
  are Whisper-native (line 203-213). Result shape defined at line 216-221.
- **Chunking processor** `processors/transcribe.py`: `DEFAULT_CHUNK_SECONDS = 60`
  (line 19), fixed-window slicing via `ffmpeg_slice` (line 98), per-chunk offset
  re-basing of `start/end` and `words[]` (line 112-128), `MAX_CHUNKS` guard
  (line 86), and `maybe_redact` integration (line 80, 140).
- **Modal GPU transcribe** `infra/modal/orpheus_transcribe.py`: `@app.cls` +
  `@modal.enter` model preload (line 57-62), `@modal.concurrent`, `@modal.fastapi_endpoint`
  with shared-secret auth (line 146-161), scale-to-zero (`min_containers=0`,
  line 50). `large-v3-turbo` default, `float16`, model cache Volume.
- **Diarize Modal service** `infra/modal/orpheus_diarize.py` is the reference
  pattern for a *new* GPU service: CUDA/ECAPA image, energy-VAD windowing
  (line 110-117), `@modal.method` + fastapi endpoint. A WhisperX aligner mirrors
  this structure.
- **Downstream consumers** of word times: `export.subtitles`
  (`processors/audio_ops.py:178`) and `streaming.py` (`_flatten_words`, line 62).

## 4. Proposed design

### 4.1 Forced-aligned word timestamps (#298)

Add a new optional alignment step that takes the transcript segments + the source
audio and returns frame-accurate word boundaries via a wav2vec2 CTC forced
aligner (the WhisperX approach), keyed by language.

- **Where it runs.** A **new Modal GPU service** `infra/modal/orpheus_align.py`
  mirroring `orpheus_diarize.py`: `@app.cls(gpu="a10g", min_containers=0,
  scaledown_window=300)`, `@modal.enter` loads a `torchaudio`/`transformers`
  wav2vec2 model per language (cached on a `orpheus-align-cache` Volume),
  `@modal.method align(payload)` + `@modal.fastapi_endpoint(method="POST")` with
  the same `ORPHEUS_MODAL_SHARED_SECRET` token check (copy `orpheus_diarize.py:173-183`).
  Request: `{token, audio_b64 (16 kHz mono wav), language, segments:[{start,end,text}]}`.
  Response: `{segments:[{start,end,text,words:[{word,start,end,confidence}]}],
  model_version_id, gpu_seconds}`.
- **Worker wiring.** New module `apps/workers/src/orpheus_workers/align.py` with
  `get_aligner()` selection mirroring `diarize.py:142` `get_diarizer()`:
  `ModalAligner` when `ORPHEUS_ALIGN_BACKEND=modal` + `ORPHEUS_MODAL_ALIGN_URL`/
  `ORPHEUS_MODAL_ALIGN_TOKEN` are set, else a no-op `PassthroughAligner` that
  returns Whisper-native words unchanged (so CPU-only deploys still work).
- **Integration.** `word_timestamps` already flows through the transcribe
  processor. Add `params.alignment` (`"forced"` | `"whisper"`, default `"whisper"`).
  When `"forced"`, after decode/chunk-merge, call `get_aligner().align(wav_path,
  segments, language)` and replace `segments[].words`. The result shape is
  unchanged — only word `start/end/confidence` improve. Offsets for chunked files
  are re-based exactly as today (`processors/transcribe.py:119-127`).
- **Cost.** Return `gpu_seconds`; meter via the existing
  `ORPHEUS_WORKER_GPU_COST_USD_PER_SECOND` path used by the Modal transcribe/diarize
  calls.

### 4.2 ITN + smart formatting (#299)

- **Where it runs.** Worker-CPU, pure post-processing — no model download needed
  for v1. New module `apps/workers/src/orpheus_workers/formatting.py` exposing
  `format_transcript(transcript: dict, opts: dict) -> dict` that rewrites
  `text` and `segments[].text` (and re-tokenizes `words[]` spans to keep them
  aligned). v1 uses a deterministic rule engine (regex + a number-words parser)
  covering: cardinal/ordinal numbers, currency, dates, times, percentages,
  phone-like groupings, and sentence-case truecasing. v2 optionally delegates to
  an ML ITN model (`nemo-text-processing`/WFST) behind an optional extra, selected
  like `get_detector()` in `redact.py:104`.
- **Ordering.** Runs **after** alignment and **before** `maybe_redact`
  (`processors/transcribe.py:80`), so redaction regexes in `redact.py` see the
  normalized "written" numbers (better credit-card/SSN/phone matching by
  `_CC`/`_SSN`/`_PHONE`, `redact.py:23-25`).
- **API.** `params.formatting = {enabled: bool, itn: bool, punctuation: bool,
  truecase: bool}` (all default false → zero behavior change when omitted).
- **Word re-mapping.** When ITN merges tokens ("five" "dollars" → "$5"), map the
  new token's `start` to the first source word and `end` to the last, preserving
  monotonic word times for subtitles/streaming.

### 4.3 VAD-segmented long-file chunking (#395)

Replace the fixed-window loop (`processors/transcribe.py:85-133`) with
speech-aware segmentation:

- **Algorithm.** Run an energy/Silero VAD over the 16 kHz wav to get speech
  regions, then greedily pack regions into chunks of ≤ `chunk_seconds` that
  **only cut at silence** (never inside a speech region). Reuse the energy-VAD
  approach already proven in `streaming.py:_trailing_silence` (line 234-244) and
  `orpheus_diarize.py:110-117` (RMS-vs-global-floor gating). Silero VAD is an
  optional upgrade behind an extra.
- **Boundaries.** Each chunk carries its absolute `offset`; segment/word offset
  re-basing is unchanged (`processors/transcribe.py:112-128`). Keep the
  `MAX_CHUNKS` guard (line 86) and `parse_chunk_seconds` validation (line 57).
- **Silence skipping.** Regions below the VAD floor are not sent to Whisper at
  all → fewer decode seconds and no hallucinated text on silence.
- **Compatibility.** `chunk_seconds` becomes a *maximum* chunk length rather than
  a hard grid; short files (`duration <= chunk_seconds`, line 78) still take the
  single-pass path. Add `params.chunking = "vad" | "fixed"` (default `"vad"`;
  `"fixed"` preserves today's behavior for reproducibility).

### 4.4 Code-switching / per-segment language (#321)

- **Detection.** After transcription, run per-segment language ID on segments
  longer than a threshold. Two implementations behind one selector: (a) reuse the
  faster-whisper `detect_language` on each segment's audio slice (CPU, cheap), or
  (b) when the Modal transcribe service is enabled, add a `detect_language`
  branch to `orpheus_transcribe.py`'s payload handler returning per-window
  language. Default (a).
- **Result shape.** Add optional `segments[].language`. Top-level `language`
  stays the dominant language (backward compatible). Add `languages: [{code,
  ratio}]` summary at the top level, only when > 1 language is present.
- **Interaction with forcing.** When `params.language` is unset (auto),
  per-segment detection runs; when a language is forced, per-segment detection is
  skipped (respecting the caller). New `params.multilang = true` opt-in enables
  the code-switch path even under a forced primary language.

### 4.5 Production hardening (all four features)

- **Error handling & failure modes.** Every new step is wrapped so a failure
  *degrades* rather than fails the job. `get_aligner()` returns `PassthroughAligner`
  (Whisper-native words) whenever the Modal align service is unreachable, times out
  (`ORPHEUS_MODAL_ALIGN_TIMEOUT_S`, default 30), returns non-200, or is missing a
  per-language model — the transcript still ships with `alignment="whisper"` in the
  result and a `warnings[]` entry noting the fallback. Modal calls use the same
  bounded retry-with-jitter policy as `_transcribe_modal` (retry on 5xx/timeout,
  max `ORPHEUS_MODAL_ALIGN_RETRIES` default 2, no retry on 401/4xx). ITN
  (`format_transcript`) is wrapped so any parse/regex exception returns the
  un-normalized transcript plus a warning, never a 500. VAD chunking falls back to
  fixed-window (`chunking="fixed"`) if the VAD pass yields zero speech regions on a
  file with non-trivial RMS (guards against a broken VAD floor silently dropping a
  whole file). Per-segment language ID failures leave `segments[].language` unset
  and keep the top-level `language`. A poison job (corrupt/truncated audio) that
  fails all retries lands in the existing worker dead-letter path, not an infinite
  requeue.
- **Scale & concurrency / bounded cost.** The align service mirrors the transcribe
  service's `@modal.concurrent` + `min_containers=0` + `scaledown_window=300`, with
  a per-container concurrency cap and `max_containers` ceiling
  (`ORPHEUS_MODAL_ALIGN_MAX_CONTAINERS`) so a burst can't fan out unboundedly.
  Alignment payloads are capped at `chunk_seconds` of audio per call (chunked files
  align chunk-by-chunk), bounding GPU memory and giving natural backpressure. A
  per-job wall-clock budget (`ORPHEUS_ALIGN_MAX_GPU_SECONDS`) aborts alignment and
  falls back to passthrough rather than running away on a pathological file. VAD and
  ITN run inside the existing worker CPU concurrency limits; ITN is O(tokens) and
  adds no network hop.
- **Multi-tenant security & RLS.** Audio sent to the align service is transient
  (base64 in-request, never persisted on the Volume, which holds only model
  weights). The shared-secret token check (`orpheus_diarize.py:173-183`) is
  mandatory; requests without a valid `ORPHEUS_MODAL_ALIGN_TOKEN` get 401. All
  enriched result JSON is written back through the existing job/artifact path, which
  is already `org_id`-RLS-scoped — no new tables, so no new RLS surface, but the PRD
  asserts these features must not widen any tenant's read/write scope.
- **On-wire backward compatibility.** The result contract
  (`{text, segments, language, duration_seconds}`, optional `segments[].words`)
  is strictly additive: `segments[].language`, top-level `languages[]`, and
  `warnings[]` appear only when relevant, and every new `params.*` defaults to
  today's behavior (`alignment="whisper"`, `formatting` all-false, `chunking="vad"`
  is the only default *change* and is gated by the Phase-0 regression bar below).
  `segments[].words` keep the same field names and monotonic ordering, so
  `export.subtitles` (`audio_ops.py:178`) and `streaming.py` (`_flatten_words`,
  line 62) consume enriched transcripts unchanged.
- **Observability.** Emit per-feature structured metrics on the existing worker
  metrics path: `align_gpu_seconds`, `align_fallback_total{reason}`,
  `align_boundary_ms` (sampled), `itn_rewrites_total{type}`, `itn_fallback_total`,
  `vad_chunks_total`, `vad_silence_seconds_skipped`, `chunk_mid_word_cuts` (should
  be 0), `langid_segments_total`, `langid_multilang_jobs_total`. Every fallback logs
  once at WARN with the job id and reason. Alignment and langid decisions carry the
  `model_version_id` into the result for auditability.
- **Cost metering.** `gpu_seconds` from the align/detect-language Modal responses is
  metered through the existing `ORPHEUS_WORKER_GPU_COST_USD_PER_SECOND` path used by
  transcribe/diarize; passthrough/CPU paths meter zero GPU. `cost_per_job_usd` in
  the processor manifest reflects the added alignment tier.
- **Config / env surface.** See §4.6.

### 4.6 Env / config surface

`ORPHEUS_ALIGN_BACKEND` (`modal`|`none`), `ORPHEUS_MODAL_ALIGN_URL`,
`ORPHEUS_MODAL_ALIGN_TOKEN` (parallel to the diarize/transcribe trio in
`transcribe.py:56-57` and `diarize.py:121-122`), plus the hardening knobs:
`ORPHEUS_MODAL_ALIGN_TIMEOUT_S`, `ORPHEUS_MODAL_ALIGN_RETRIES`,
`ORPHEUS_MODAL_ALIGN_MAX_CONTAINERS`, `ORPHEUS_ALIGN_MAX_GPU_SECONDS`,
`ORPHEUS_ITN_ENGINE` (`rules`|`nemo`), `ORPHEUS_VAD_ENGINE` (`energy`|`silero`).
All are read once at worker start and surfaced in the startup config log; missing
optional vars fall back to the CPU-only defaults so a key-less deploy is fully
functional.

## 5. Delivery milestones

Milestones are **ordering only** — each one is production-quality, fully hardened
per §4.5, and independently shippable. None is a reduced-scope prototype; the
complete feature set of this PRD is in scope and every milestone ships with error
handling, observability, cost metering, and the acceptance bar in §6 met for its
slice.

- **M0 — Baselines & harness.** Build the golden corpus (30 utterances with
  hand-labeled word boundaries; a 60-min file; en↔es/en↔hi code-switch sets) and
  the automated regression harness that computes WER, boundary error, and
  mid-word-cut count. This is the gate every later milestone's acceptance runs
  against.
- **M1 — VAD-segmented chunking (#395), production-complete.** Worker-CPU change
  with `params.chunking="vad"` default and `"fixed"` for reproducibility, full
  fallback-to-fixed on empty-VAD, silence-skip metering, and the mid-word-cut and
  WER-non-regression gates green on the golden file.
- **M2 — ITN + smart formatting (#299), production-complete.** Deterministic rule
  engine (ITN, truecasing, punctuation) with exception-safe fallback, word
  re-mapping keeping `words[]` monotonic, ordered before `maybe_redact`, opt-in via
  `params.formatting`. Optional `nemo` engine behind `ORPHEUS_ITN_ENGINE`.
- **M3 — Forced alignment (#298), production-complete.** Deploy `orpheus_align.py`
  Modal service + `align.py` worker module with full retry/timeout/budget handling,
  passthrough fallback, GPU cost metering, and the ≤ 50 ms boundary bar green.
- **M4 — Code-switching / per-segment language (#321), production-complete.**
  Per-segment language ID, additive `segments[].language` + `languages[]` summary,
  respecting forced-language callers, with the ≥ 90% label-accuracy bar green and a
  runbook for the new Modal service.

## 6. Verification / acceptance criteria

A senior QA runs these **end-to-end against a real worker + a live Modal
deployment** (not unit-only). Each numeric target below is a hard gate.

1. **VAD chunking (happy path):** submit a 90-min file with `params.chunking="vad"`;
   assert 0 segments whose text is split mid-word at a former 60 s boundary
   (`chunk_mid_word_cuts == 0`); assert total Whisper decode seconds < fixed-window
   run (silence skipped, `vad_silence_seconds_skipped > 0`); assert WER ≥ 1.0
   absolute point better than fixed-window on the 60-min golden file and never
   worse elsewhere.
2. **VAD failure path:** feed a file whose VAD yields zero speech regions despite
   non-trivial RMS (e.g. a mis-tuned floor); assert the job auto-falls-back to
   `"fixed"`, still completes, emits a `warnings[]` entry, and increments the
   fallback metric — no dropped audio, no 500.
3. **Fixed-mode parity:** same file with `chunking="fixed"` reproduces the current
   segment count/offsets byte-for-byte (regression guard for reproducibility).
4. **Forced alignment (happy path):** submit `word_timestamps=true,
   alignment="forced"` against the live align service; assert median |Δboundary|
   ≤ 50 ms vs. golden, `gpu_seconds` recorded and billed via the GPU-cost path, and
   `model_version_id` present in the result.
5. **Forced alignment (failure paths):** (a) stop the align Modal service mid-run →
   assert graceful passthrough (`alignment` reported as `"whisper"`, `warnings[]`
   set, job completes); (b) inject a 401 (bad token) → assert no retry storm and
   clean fallback; (c) submit a language with no aligner model → assert passthrough,
   not error; (d) exceed `ORPHEUS_ALIGN_MAX_GPU_SECONDS` on a pathological file →
   assert budget abort + fallback.
6. **ITN (happy path):** transcript containing "twenty twenty six", "five dollars",
   "three thirty pm" with `formatting={itn:true}` yields "2026", "$5", "3:30 pm";
   `words[]` remain monotonic and each merged token's `start`/`end` spans its source
   words.
7. **ITN failure path:** feed input that trips a rule-engine exception (malformed
   segment); assert the un-normalized transcript is returned with a warning, the
   job succeeds, and `itn_fallback_total` increments.
8. **ITN→redaction ordering:** spoken credit-card digits become grouped digits and
   are then masked by `text.redact` (Luhn check in `redact.py:31` still passes),
   proving formatting runs before `maybe_redact`.
9. **Code-switch:** en↔es file returns per-segment `language`, correct on ≥ 90% of
   segments; top-level `language` is the dominant one; single-language files carry
   no `languages[]` key (back-compat); a forced-language job with `multilang=false`
   emits no per-segment language (caller respected).
10. **Concurrency / backpressure:** drive N concurrent alignment jobs above the
    per-container cap; assert containers scale within `max_containers`, no request
    is dropped, latency degrades gracefully, and cost metering sums correctly across
    jobs.
11. **Multi-tenant isolation:** two orgs submit concurrently; assert each job's
    result is written only under its own `org_id` (RLS), and no audio persists on the
    align Volume after the run.
12. **Shape stability:** existing consumers `export.subtitles` and `streaming.py`
    run unchanged against enriched transcripts; a client reading only the original
    four fields sees no breakage.

## 7. Dependencies, risks, open questions

- **Dependencies:** new Modal service + Volume + `orpheus-modal-auth` secret reuse;
  `transformers`/`torchaudio` for wav2vec2; optional `nemo-text-processing`/`silero-vad`
  extras in `pyproject.toml`.
- **Risks:** (a) wav2vec2 alignment needs per-language models — missing-language
  fallback must be passthrough, not error. (b) ITN over-correction on already-written
  text — gate with conservative rules + golden regression set. (c) VAD packing on
  music/noise could under-segment — keep `MAX_CHUNKS` and a max-chunk-length cap.
- **Open questions:** ship WhisperX as a bundled worker dep (heavier image) vs.
  Modal-only? Cost ceiling for forced alignment per minute? Do we persist
  `segments[].language` into the DB result JSON only, or also surface in
  `export.subtitles`?

## 8. Effort

**T-shirt: L** (≈ 5–7 engineer-weeks; each milestone includes its hardening +
acceptance bar, not just the happy path).

- M0 (0.5 wk): golden corpus + automated regression harness.
- M1 (1 wk): production VAD chunking incl. empty-VAD fallback + metrics.
- M2 (1 wk): ITN/formatting module + ordering + exception-safe fallback.
- M3 (2 wk): `orpheus_align.py` Modal service + `align.py` + retry/timeout/budget +
  passthrough fallback + GPU metering.
- M4 (1 wk): code-switching per-segment language + docs/runbook.
