# PRD: Caption styling & burn-in + local-dev streaming

**Status:** Proposed · **Priority:** P2 (captions) / P3 (dev-ops) · **Epic:** Exports & DX · **Related issues:** caption styling/burn-in (FEATURES-AND-ISSUES A11), streaming-in-`make dev` (A2)

## 1. Summary

Two remaining checklist items that don't fit the other 15 epics: (a) **styled subtitles + burned-in captions** — richer SRT/VTT (speaker labels, colours, positioning) and a rendered video with captions burned into the pixels; and (b) a **dev-ops ergonomics fix** so the streaming ASR server starts with the rest of the stack under `make dev` instead of being a separate, easy-to-miss process. Grouped here so the backlog has zero uncovered items.

Both are specified as **complete, production-grade** features — the caption work ships tenant-ready (RLS-scoped artifacts, retention/ZDR-honoring, bounded transcode cost, observable, metered), and the dev-ops fix ships as a robust, self-healing part of the local stack. Delivery is ordered into **Milestones (M1–M3)**; each milestone is itself production-quality and shippable, not a reduced-scope prototype.

## 2. Motivation & goals

- **Problem (captions):** `export.subtitles` emits plain SRT/VTT only. Media/meeting users expect speaker-labelled, styled captions and — for social/marketing clips — captions **burned into** the video so they render everywhere without a sidecar file. Competitors (Descript, meeting tools) ship this.
- **Problem (dev-ops):** the streaming server (`orpheus-streaming` on :8082) is a separate process; a contributor running `make dev` gets the API + workers but not streaming, so `/stream/*` silently fails until they discover the missing process (noted in A2 and `streaming_ws.go` operational comments).
- **Goals:** styled SRT/VTT export options; a burned-in caption artifact for video inputs; `make dev` brings up streaming too; all verifiable e2e, all production-grade (bounded cost, defined failure modes, multi-tenant isolation, observability).
- **Non-goals:** a caption *editor* UI (that's the dashboard's job); animated/karaoke word-by-word highlighting (future); TTS/dubbing (see prd-06).

## 3. Current state in Orpheus

- **Subtitle export:** `export.subtitles` processor in `apps/workers/src/orpheus_workers/processors/audio_ops.py` (registered ~`audio_ops.py:175`, `model_id="subtitle-render"`) builds SRT/VTT from a transcript's `segments` (and `speaker` when diarized). Output is `{formats, artifacts}` — plain text subtitle artifacts uploaded to S3.
- **Word/speaker data available:** transcribe now emits word timestamps; `audio.diarize` emits per-segment `speaker`. So styling inputs (speaker, timing, words) already exist in the result shape.
- **ffmpeg helpers:** `apps/workers/src/orpheus_workers/ffmpeg.py` wraps ffmpeg (`convert_to_wav_16k_mono`, `slice`); `processors/slice.py` and `processors/transcribe.py:11` already shell out to ffmpeg. Burn-in is another ffmpeg invocation (`-vf subtitles=...`).
- **Artifact/upload path:** processors download the source artifact, produce output, and `s3.upload_file` + `db.insert_artifact` a new artifact (pattern in `slice.py`, `audio_ops.py`). Artifacts are org-scoped and flow through the same retention/erasure/ZDR governance as any other output.
- **Local dev:** root `Makefile` `dev` target starts Postgres/NATS/minio + API + `uv run --package orpheus-workers python -m orpheus_workers.worker`, but **not** `orpheus-streaming` (console script → `orpheus_workers.streaming:main`). The streaming server reads `ORPHEUS_STREAMING_PORT` (default 8082) and the relay dials `ORPHEUS_STREAMING_WS_URL`.

## 4. Proposed design

### 4a. Styled subtitles (extend `export.subtitles`)

Add optional `params` to the existing processor (no new processor needed). The field is **additive and backward-compatible** — omitting `style` reproduces today's exact output byte-for-byte:

```jsonc
{
  "formats": ["srt", "vtt"],          // existing
  "style": {
    "speaker_labels": true,            // prefix cues with "S1:" / mapped names
    "speaker_names": {"S1": "Alice"}, // optional label map
    "max_line_chars": 42,              // re-wrap long cues
    "max_lines": 2,
    "vtt_cue_settings": {"position": "50%", "align": "center"},
    "vtt_styles": {"S1": {"color": "#7CE0FF"}}   // ::cue voice styling in VTT
  }
}
```

- **SRT:** speaker prefix + re-wrapping (SRT has no colour; keep it clean).
- **VTT:** emit a `STYLE` block with `::cue(v[voice="S1"])` colour rules and per-cue `<v S1>` voice spans + cue-setting lines — the standard WebVTT styling mechanism.
- Implementation lives in a `subtitles.py` helper the processor already conceptually owns; result shape unchanged plus an additive `style_applied` field (`{formats, artifacts, style_applied}`).
- **Validation & failure modes.** All `style` params are validated (colours are well-formed, `max_line_chars`/`max_lines` are bounded positive ints, `speaker_names` keys reference labels present in the transcript). Invalid params fail the job at validation with a clear `ValueError`/code rather than emitting malformed subtitles. A transcript lacking `speaker` data with `speaker_labels: true` degrades gracefully to un-prefixed cues (and reports it in `style_applied`) instead of failing. Rendering is deterministic (no model) so output is reproducible.
- **Governance/observability.** Output artifacts are org-scoped (RLS) and honor the source job's retention/ZDR policy; emit metrics on styled-export counts and cue-count/line-length distributions.

### 4b. Burned-in captions (new `export.captions-burn` processor)

New processor `export.captions-burn` (tier `cpu_small`, `model_id="caption-burn"`), registered like the others:

1. Resolve the transcript (via `source_job_id`) and the **source video** artifact (`artifact_id`); if the source is audio-only, fail with a clear `ValueError` (burn-in needs video).
2. Render a styled `.ass`/`.srt` to a temp file (reuse 4a's renderer; `.ass` gives positioning/colour control).
3. `ffmpeg -i input.mp4 -vf "subtitles=subs.ass" -c:a copy out.mp4` (add `force_style=` for global styling). Use the existing ffmpeg wrapper; add a `burn_subtitles(src, subs, dst, style)` function to `ffmpeg.py`.
4. Upload the muxed video as a new artifact (content-type from the source), return `{artifact_id, size_bytes, format}`.

- **Cost & scale (bounded).** Burn-in is transcode-bound wall-clock CPU. Cost is bounded by: an input duration/size cap (oversize inputs rejected with a clear code), exposed CRF/preset params, and the existing chunk/timeout guards (`processors/slice.py` pattern). The processor runs on the flat CPU path; if throughput demands, it can later move to a bounded Modal service (mirror `orpheus_transcribe.py`). Concurrency is bounded by the worker pool so transcodes apply backpressure rather than exhausting host CPU/disk.
- **Failure modes.** Audio-only source → clean `ValueError` dead-letter. ffmpeg non-zero exit (bad codec, corrupt input, missing font) is captured with stderr context and dead-lettered with an actionable reason, never a truncated/partial artifact. Temp files are cleaned up on both success and failure paths. Non-Latin scripts render via bundled Noto fonts (see risks); a missing glyph degrades to a tofu box, not a crash.
- **Governance/observability.** Output video artifact is org-scoped (RLS), honors source retention/ZDR; metrics on transcode duration, output size, and failure reasons; per-job wall-clock metered through the usage rails so burn-in is cost-accounted.

### 4c. Local-dev streaming (dev-ops)

- Add a `dev-streaming` Make target (`ORPHEUS_STREAMING_PORT=8082 uv run --package orpheus-workers orpheus-streaming`) and include it in the `make dev` process group (background it alongside the worker, or add to the process manager / `foreman`/`overmind` Procfile if one exists).
- Have `make dev` print the streaming URL and a health check (`curl :8082/health`) so a missing server is obvious; the target waits for `/health` to return ok (bounded retry) before declaring the stack up, so a failed/slow streaming start surfaces immediately instead of silently.
- Ensure clean teardown: the streaming process is stopped with the rest of the `make dev` group (no orphaned :8082 listener); a port-in-use condition prints an actionable message rather than a stack trace.
- Document in the repo README's local-dev section, including the health endpoint and env vars (`ORPHEUS_STREAMING_PORT`, `ORPHEUS_STREAMING_WS_URL`).

## 5. Rollout / milestones

Each milestone is independently production-quality and shippable. Milestones order the work; none is a reduced-scope prototype.

1. **M1 — styled SRT/VTT** (4a): speaker labels + re-wrapping + VTT colour, with full param validation, graceful degradation for missing speaker data, RLS/retention-honoring artifacts, and metrics. Additive/backward-compatible — no `style` reproduces today's output exactly.
2. **M2 — burn-in** (4b): `export.captions-burn` + `ffmpeg.burn_subtitles`, with bounded input size/duration, CRF/preset controls, robust ffmpeg-failure dead-lettering, temp cleanup, bundled fonts, and metered transcode.
3. **M3 — dev-ops** (4c): `make dev` starts streaming with a health-gated startup, clean teardown, and README docs.

## 6. Verification / acceptance criteria

End-to-end against a **real worker + Go API** (and the real streaming server + relay for 4c), with negative/failure paths and multi-tenant isolation checks.

- **Styled SRT/VTT.** Run `export.subtitles` with `style.speaker_labels` on a diarized transcript → VTT contains a `STYLE` block, `<v S1>` voice spans, and speaker-prefixed cues; SRT has speaker prefixes and no cue exceeds `max_line_chars`. Diff against a golden file (deterministic — no model). **Additive proof:** the same job with no `style` produces byte-identical output to the pre-change processor. **Negative paths:** invalid colour / non-positive `max_line_chars` / unknown `speaker_names` key → job fails at validation with a stable error code (no malformed artifact written); `speaker_labels: true` on a non-diarized transcript degrades to un-prefixed cues and reports it in `style_applied`. **Isolation:** the styled artifact is readable only within its org (RLS test).
- **Burn-in.** Submit `export.captions-burn` on a short MP4 with a transcript → output artifact is a valid MP4 (`ffprobe` shows a video stream, audio copied) that is visibly re-encoded; a frame grab (ffmpeg `-vframes 1`) shows the caption text (OCR or manual QA check). **Negative/failure paths:** audio-only source → clean `ValueError` dead-letter; a corrupt/oversize input → dead-letter with an actionable reason and **no** partial artifact; a non-Latin-script transcript renders visible glyphs via bundled fonts. **Cost/scale:** a job exceeding the input duration/size cap is rejected; concurrent burn-in jobs apply backpressure without exhausting the worker host. **Governance:** the output honors the source job's retention/ZDR policy; transcode wall-clock is metered.
- **Dev-ops.** Fresh `make dev` → `curl 127.0.0.1:8082/health` returns `{"status":"ok"}` without starting anything by hand; startup is **health-gated** (the stack does not report ready until streaming answers `/health`). A streaming session (create → WS → finalize) works end-to-end. Teardown of `make dev` leaves no orphaned :8082 process; a pre-occupied port produces an actionable message, not a crash.

## 7. Dependencies, risks, open questions

- **Deps:** ffmpeg with `libass` (subtitles filter) — confirm the worker image bundles it (`apps/workers/Dockerfile`); `.ass` rendering needs no extra Python dep. Bundled Noto fonts for non-Latin scripts must be present in the image (add if absent).
- **Risks:** burn-in re-encodes video (CPU/time cost + potential quality loss) — mitigated by exposed CRF/preset params, input size/duration caps, and the existing chunk/timeout guards. Font availability inside the container for non-Latin scripts (bundle Noto fonts). ffmpeg failure surfaces are broad — captured with stderr context and dead-lettered, never producing partial artifacts.
- **Open questions:** default caption style (position/size); whether to also emit `.ass` as a first-class subtitle format; whether burn-in should support word-level highlight (defer); the exact input duration/size cap and default CRF/preset for the bounded-cost transcode path.

## 8. Effort

- **Styled SRT/VTT:** S–M (extend one processor + renderer + validation + golden tests).
- **Burn-in:** M (new processor + one ffmpeg helper + video handling + bounded-cost/failure hardening).
- **Dev-ops:** XS–S (Makefile health-gated target + teardown + docs).
- **Milestones:** M1 styled export → M2 burn-in → M3 `make dev` streaming, each shippable on completion.
