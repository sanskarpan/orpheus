# PRD 05 — Speaker diarization + word-level timestamps + subtitle (SRT/VTT) export

**Status:** Draft · **Owner:** Processors & Models · **Reviewers:** ML, Platform
**Related:** PRODUCTION_DESIGN §5.4, §6.1 (diarize+align is a Temporal workflow), §7.2 (result
carries `speaker` per segment) · IMPLEMENTATION_STATUS: `diarize` is ❌, GPU is ❌ today

## 1. Problem & motivation

The result schema already anticipates diarization — segments carry a `speaker` field — but the
`diarize` processor and alignment are not implemented, and transcripts today are segment-level,
not word-level. Customers building captioning, meeting notes, and searchable media need three
tightly-related capabilities: **who spoke** (diarization), **exactly when each word was said**
(word-level timestamps), and **standard subtitle files** (SRT/VTT). These are the highest-demand
transcript enrichments and unblock PRD 04 (translate) → subtitle pipelines.

## 2. Goals / non-goals

**Goals**
- `orpheus.audio.diarize` — assign speaker labels (`S1`, `S2`, …) to transcript segments/words.
- Word-level timestamps as an option on `transcribe` (and a standalone `align` step).
- `orpheus.export.subtitles` — emit `.srt` and `.vtt` from a transcript, with speaker labels,
  configurable max chars/line and max lines/cue.

**Non-goals**
- Speaker *identification* (naming real people / voiceprints) — labels are anonymous S1..Sn in v1.
- Live/streaming diarization.
- Burned-in captions on video (we output sidecar files, not muxed media).

## 3. User stories

- As a podcast tool, I want per-speaker transcripts so I can render a two-column conversation.
- As a captioner, I want a `.vtt` I can drop into an HTML5 `<video>` with speaker names.
- As a search product, I want word-level timestamps so clicking a word seeks the audio.

## 4. Proposed API / UX

Processors on the existing `POST /v1/jobs`:

```jsonc
// Transcribe with word timestamps + inline diarization
{ "artifact_id": "<audio>",
  "processor": { "name": "orpheus.audio.transcribe", "version": "1.5.0" },
  "params": { "word_timestamps": true, "diarize": true, "max_speakers": 6 } }

// Standalone diarization over an existing transcript+audio
{ "processor": { "name": "orpheus.audio.diarize", "version": "1.0.0" },
  "params": { "source_job_id": "018f...", "max_speakers": 6 } }

// Subtitle export
{ "processor": { "name": "orpheus.export.subtitles", "version": "1.0.0" },
  "params": { "source_job_id": "018f...", "formats": ["srt","vtt"],
              "include_speaker_labels": true, "max_chars_per_line": 42, "max_lines": 2 } }
```

Results extend the existing shape: `result.segments[].speaker`, new
`result.segments[].words[] = {start,end,word,confidence}`, and output artifacts
`transcript.srt` / `transcript.vtt` (downloadable via signed URL / PRD 02 bundle).

## 5. Data-model changes

- **None to core tables.** Word timestamps and speaker labels live inside `job_results.result`
  (JSONB); subtitle files are ordinary output `artifacts`.
- Register `diarize`, `subtitles`, and the new `transcribe` version in the processor catalog with
  pinned `model_version_id` (e.g. a pyannote-class model for diarization).
- Diarize + align runs as a **DB-tracked workflow** (matching current impl) — probe → transcribe →
  diarize → align → persist — reusing existing workflow tracking, not requiring Temporal for v1.

## 6. Edge cases & security

- **Tenant isolation:** source audio + transcript resolved under RLS; diarization output stored on
  the tenant's job/result rows only.
- **Compute/DoS:** diarization is heavier than transcription; enforce a **max input duration** per
  plan and count GPU-seconds toward per-tenant budgets (PRD 07). GPU is not available today, so v1
  may ship a **CPU-only** diarization model with a documented duration cap, GPU as fast-follow.
- **Privacy:** anonymous labels only; no voiceprint storage. Any future speaker-ID is a separate,
  consent-gated feature and out of scope here.
- **Subtitle injection:** subtitle text is transcript-derived and untrusted; escape/sanitize when
  writing `.vtt` (which allows limited markup) to avoid caption-based XSS in downstream players.
- **PII:** subtitles/word lists can contain PII; honor PRD 08 redaction when requested.

## 7. Metrics / SLAs

- `diarize_latency_p95`, diarization error rate (DER) on a golden set (offline eval).
- `subtitle_export_latency_p95 < 5s`. Word-timestamp coverage (% words aligned).
- Track GPU/CPU-seconds per job for cost + budgets.

## 8. Rollout plan

1. Add `word_timestamps` to `transcribe` (CPU-viable alignment) — low risk, additive.
2. Ship `export.subtitles` (pure transformation over existing transcripts).
3. Ship CPU `diarize` with a duration cap; wire the diarize+align workflow.
4. Swap to GPU diarization when the GPU worker pool + model registry land.

## 9. Open questions

- Diarization model choice given CPU-only constraint today vs. quality (DER) targets.
- Cue-splitting heuristics for readable subtitles (sentence vs. gap-based segmentation).
- Do we expose a merged "speaker-turn" view, or leave merging to the client?
