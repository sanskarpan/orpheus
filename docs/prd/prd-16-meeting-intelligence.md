# PRD: Meeting & Media Intelligence

**Status:** Proposed · **Epic:** Meeting Intelligence · **Related issues:** #357 #358 #359 #360 #361 #362 #363 #364 #365 #366 #367

## 1. Summary

Turn Orpheus from a batch media-processing API into a **meeting-intelligence platform**. On top of the shipped `transcribe` / `audio.diarize` / `text.summarize` / `text.topics` / `text.entities` processors we add: a **meeting-bot ingestion service** that auto-joins Zoom/Meet/Teams and streams audio into the existing pipeline (#357); **live notes, action items, and decisions** derived during and after the call (#358); a **transcript store + embedding index** enabling cross-meeting semantic search and a knowledge base (#359, #434); **ask-AI/chat over one or many transcripts** with citations (#360); **highlight reels / clips** (#361); **collaboration** — comments, sharing, mentions (#362); **conversation intelligence / scorecards** (#363); **CRM auto-fill** (#364); and a Descript-class **audio-edit-by-text**, **filler-word removal**, and **overdub/voice-clone/dubbing** track (#365, #366, #367).

This PRD is the umbrella; the transcript store + retention + region details are specified in `prd-12-data-lifecycle.md`, and the on-device / STS-translation / alignment model work in `prd-14-model-deployment.md`. Here we own the ingestion service, the meeting/session data model, and the product surfaces.

## 2. Motivation & goals

Today a customer must upload a file and poll a job. Competitors (Otter, Fireflies, Fathom, Gong, Descript) win by removing the upload step (bots), by making transcripts a durable searchable corpus, and by layering coaching/CRM/editing on top. Orpheus already has the hard parts — diarized, word-timestamped transcripts (`export.subtitles` proves word-level cues exist), a provider-agnostic LLM layer, GPU offload on Modal, and multi-tenant RLS. We are missing the *product* around them.

**Goals**
- Auto-join a scheduled meeting and produce a diarized, summarized, action-item-tagged record with **zero manual upload**.
- One org-scoped **transcript corpus** that is searchable by keyword and by meaning, and chat-queryable with citations back to `segment.start`.
- Ship editing (text-driven cut, filler removal) and synthesis (overdub/dub) as **new processors**, reusing the job model and artifact/bundle delivery.
- Every surface stays inside the existing auth (`X-API-Key` scopes), RLS, outbox→NATS→webhook, and usage-metering rails.

**Non-goals**
- Real-time (<2s) live captions in the meeting UI — the streaming ASR socket (`/stream/transcribe`, `streaming.go`) already covers low-latency; "live notes" here means near-real-time (chunked, ~10–30s).
- Replacing the CRM — we push structured fields out, we are not a CRM.
- Real voice cloning without consent gating (see risks).

## 3. Current state in Orpheus (file:line, patterns to build on)

- **Processors registry.** `apps/workers/src/orpheus_workers/processors/__init__.py:55` `register_processor`; manifests mirror to DB via `catalog.sync_catalog`. Adding a processor = one decorated async fn `(ctx, job_id) -> dict`.
- **Transcribe.** `processors/transcribe.py:22` returns `{text, segments, language, duration_seconds}`; word timestamps via `params.word_timestamps` (`transcribe.py:54`). GPU path offloads to Modal when `ORPHEUS_WORKER_TRANSCRIBE_BACKEND=modal` (`transcribe.py:127,181`).
- **Diarization.** `processors/audio_ops.py:122` `audio.diarize` (S1..Sn), and `audio_ops.py:178` `export.subtitles` (word-level SRT/VTT builders are pure fns, `audio_ops.py:47`).
- **LLM layer.** `apps/workers/src/orpheus_workers/llm.py` — task-shaped `get_llm()`, `summarize/translate/detect_language/complete`, provider-agnostic (`stub`/`anthropic`/`openai`/`gemini`/`openai-compat`), Modal-hosted open model at `infra/modal/orpheus_llm.py`. Summarize already supports `mode=action_items` (`text_ops.py:139`). Topics/entities: `text_ops.py:251,280`.
- **Streaming control plane.** `handlers/streaming.go` — `POST /v1/streaming/sessions` mints a short-lived token; WS relay `/stream/transcribe` (`server.go:257`) authenticates outside the API-key middleware because browsers can't set headers on a WS handshake. This is the exact shape a bot's audio relay reuses.
- **Job create / dispatch.** `handlers/jobs.go` — `_processor` reserved key in `params` jsonb (`jobs.go:120`), `202` + `poll_url`, content cache (`cache.go`), outbox events (`jobs.go:36`).
- **Delivery / bundles.** `export.bundle` (`processors/export_bundle.py:27`) → signed expiring zip; `internal/delivery/delivery.go` pushes results to tenant S3.
- **RLS.** `internal/db/db.go:81` `set_config('app.current_org_id', $1, true)` per tx; every new table is org-scoped + FORCE RLS (goose migrations `apps/api/internal/db/migrations/`, latest `0020_marketplace.sql`).

## 4. Proposed design

### 4.1 Meeting-bot ingestion service (#357)
A new deployable, `apps/meetingbot/` (Go, mirrors the API's package style), plus per-provider headless join workers. Flow:
1. `POST /v1/meetings` `{provider: zoom|meet|teams, join_url|meeting_id, calendar_event_id?, bot_name?, record_video?}` → creates a `meetings` row (`status=scheduled`), scope `meetings:write`. A companion **calendar connector** (OAuth per provider, stored per-org) can auto-schedule joins from a synced calendar.
2. At start time the ingestion service launches a headless bot container (Modal function or a container host — Modal's scale-to-zero + shared-secret auth pattern, `infra/modal/orpheus_transcribe.py`, fits the bursty join workload). The bot admits itself and captures the mixed audio track (and optionally per-participant tracks where the platform exposes them).
3. The bot **reuses the streaming relay**: it opens `/stream/transcribe` with a session token minted exactly like `StreamingHandler.Create`, streaming PCM chunks. Near-real-time partials feed "live notes"; the final finalize (`streaming.go` `Finalize`) persists the full transcript.
4. On end, an orchestration workflow (DB-tracked, like `workflows.go` `transcribe-long`) fans out `audio.diarize` → `text.summarize(mode=action_items|chapters)` → `text.topics` → `text.entities`, and writes a **meeting record**.

Consent/recording-notice is enforced by the bot (announce + configurable per-region requirement) — see risks.

**Error handling & failure modes (graceful degradation).** Every failure leaves a durable, queryable record — never a silent drop:
- **Bot fails to join / admitted-then-kicked / headless-join crash.** The join is a bounded-retry step in the ingestion workflow (exponential backoff, capped attempts) with a per-provider join timeout. If the bot never gets in, `meetings.status=join_failed` with a `failure_reason` and the outbox emits `meeting.failed`; the calendar connector may reschedule a retry join. If the bot is admitted then kicked/crashes mid-call, whatever PCM was already streamed through `/stream/transcribe` is finalized (`streaming.go` `Finalize`) so a **partial transcript is always persisted**, and the meeting is marked `status=degraded` with `degraded_reason=bot_disconnected`. The post-call workflow still runs over the partial transcript; `meeting.notes` annotates that coverage is partial.
- **Platform ToS / recording block.** If a provider signals recording is disallowed (host disabled it, ToS-gated), the bot leaves immediately, sets `status=blocked`, emits `meeting.failed{reason=recording_blocked}`, and no audio is captured or stored — this is the compliant default, not an error to retry.
- **Modal TTS / embedding / transcribe failure.** All Modal-offloaded steps use the shared-secret call pattern with bounded retries; on exhausted retries the individual job transitions to `failed` (existing job lifecycle) without failing the whole meeting — diarization/notes degrade to CPU-local or `stub` fallbacks where a fallback exists (e.g. `ORPHEUS_WORKER_TRANSCRIBE_BACKEND` falls back off `modal`), and the meeting is `degraded` rather than lost. Scale-to-zero cold-start timeouts are treated as retryable.
- **LLM JSON-parse failure in `meeting.notes` / `chat.answer`.** Structured `complete()` calls are JSON-schema-constrained and validated; on malformed/invalid JSON we retry once with a repair prompt, then fall back to a plain-text `summary` note (notes never hard-fail — a meeting always gets at least a transcript + best-effort summary). `chat.answer` that cannot produce valid cited JSON returns a retrieval-only answer flagged `citations_unverified` rather than fabricating citations.
- **Partial / failed diarization.** If `audio.diarize` yields fewer speakers than participants or fails, notes and scorecards fall back to non-speaker-attributed mode (`speaker_label=unknown`); the meeting is still complete, flagged `diarization_partial`.
All workflow steps are idempotent and resumable from the DB-tracked step state so a worker restart resumes rather than restarts the fan-out.

**Scale, concurrency & bounded cost.** Bot, TTS (`infra/modal/orpheus_tts.py`), and embedding functions are Modal **scale-to-zero with an explicit `max_containers`** cap so a burst of simultaneous meetings applies backpressure (queue) instead of unbounded GPU spend. Bots are launched **join-at-start-time** (scheduled from the calendar connector / start timestamp) so no standing containers idle between meetings. The post-call orchestration workflow fans out with a **bounded degree of parallelism** per org and a global cap, reusing the `workflows.go` step model, so one org's 200-attendee all-hands cannot starve another tenant. CPU-bound steps (FTS, EDL render) and GPU-bound steps (transcribe, TTS, embedding) have separate concurrency pools. Per-org rate limits on `POST /v1/meetings` and `POST /v1/chat` bound fan-in.

**Multi-tenant security, RLS & consent.** Every new table (§4.2) is org-scoped + **FORCE RLS**, set via `set_config('app.current_org_id', …)` per tx (`internal/db/db.go:81`). Recording-notice is **enforced** by the bot (announce + per-region consent config); voice-clone/overdub requires a stored per-voice **consent record** before any synthesis runs. Per-region data-residency and retention are governed by `prd-12`. Consent config, region, and retention class are read at join time and stamped onto the `meetings`/`transcripts` rows.

**Backward-compatible, additive on-wire shape.** New processors register **additively** via `register_processor` → `catalog.sync_catalog` (no change to existing manifests). New outbox event types (`meeting.recorded`, `meeting.notes.ready`, `clip.ready`, `note.action_item.assigned`, `meeting.failed`) are **additive** — existing subscribers ignore unknown types; the job-create/`202`+`poll_url`/webhook-HMAC contract (`jobs.go`) is unchanged. No existing endpoint, event, or processor signature changes.

**Observability.** Emit metrics/logs for: bot join success rate, join latency, notes-ready latency (call-end → `meeting.notes.ready`), chat citation-validity rate, embedding index lag, clip render time, and per-processor error counts — labeled by `org_id` and processor. Failures carry structured `failure_reason`/`degraded_reason` for alerting.

**Cost metering.** GPU-seconds for bot/TTS/embedding and LLM tokens for notes/chat/scorecard are attributed **per org through the existing metering rails** (`jobs.cost_usd`, `usage_rollup`) — every bot session, Modal call, and LLM pass is a metered job so billing and per-org cost dashboards come free.

### 4.2 Data model (new goose migration `0021_meetings.sql`, all org-scoped + FORCE RLS)
- `meetings` — `id uuid7 pk, org_id, provider, external_meeting_id, title, started_at, ended_at, status, bot_session_id, calendar_event_id, video_artifact_id?`.
- `meeting_participants` — `meeting_id, display_name, email?, speaker_label (maps to diarize S1..Sn)`.
- `transcripts` — canonical store keyed by `meeting_id` **or** `job_id` (a plain uploaded file is a meeting-less transcript). Columns `id, org_id, source_job_id, meeting_id?, language, duration_seconds, segments jsonb, text tsvector-backed`. Owned in detail by `prd-12`.
- `transcript_segments` (optional denormalized) for FTS + `pgvector` embedding column — see `prd-12` for the index decision.
- `meeting_notes` — `meeting_id, kind (summary|action_item|decision|highlight), body, refs jsonb ([{segment_idx,start,end}]), status, assignee?, due?`. Action items and decisions are `kind` rows so they share comment/sharing plumbing.
- `comments` — `id, org_id, subject_type (meeting|note|segment|clip), subject_id, author_user_id, body, anchor jsonb`, for #362.
- `shares` — `subject_type, subject_id, audience (org|link|email), token?, expires_at, permission (view|comment)`.
- `clips` — `id, meeting_id, start, end, title, artifact_id (rendered mp4/mp3), transcript_slice jsonb` for #361.
- `crm_pushes` — `meeting_id, destination_id, payload jsonb, status` for #364.

### 4.3 New processors (worker side, `register_processor`)
- `meeting.notes` — post-call LLM pass producing action items + decisions + highlights in one structured `complete()` call (JSON-schema-constrained). Untrusted-transcript sandboxing follows the existing `_SUMMARIZE_SYSTEM` pattern (`llm.py`).
- `chat.answer` (#360) — RAG over the transcript index: retrieve top-k segments (semantic + keyword, `prd-12`), `llm.complete()` with citations `[{meeting_id, segment_idx, start}]`. Supports single-transcript and cross-corpus scope.
- `clip.render` (#361) — takes `{meeting_id, ranges[]}`, uses the existing ffmpeg slice helpers (`ffmpeg.slice`, used in `transcribe.py:11`) to cut audio/video, muxes captions from the word-timestamped transcript, emits an artifact.
- `conversation.score` (#363) — talk-ratio, longest monologue, question rate, sentiment (`text.sentiment`, `text_ops.py:226`), topic coverage vs a configurable scorecard template → structured metrics.
- `edit.by_text` (#365) — the transcript is the timeline; deleting words/sentences in the returned edit-doc maps back to `segment.words[].start/end`; render = ffmpeg cut-list. Non-destructive: edits stored as an EDL, rendered on demand (multitrack-friendly, #366).
- `edit.remove_fillers` (#366) — detect um/uh/like/you-know spans from word timestamps + a filler lexicon (LLM-assisted for language-specific fillers), produce an EDL removing them; optional per-track.
- `tts.overdub` (#367) — generate replacement audio for edited spans / full dubbing; a **new Modal service** (`infra/modal/orpheus_tts.py`) mirroring the transcribe/llm services (shared secret, scale-to-zero, model on a Volume). Voice-clone gated behind explicit per-voice consent records.

### 4.4 API surface (Go, chi — `handlers/meetings.go`, wired in `server.go` `v1Routes`)
- `POST /v1/meetings`, `GET /v1/meetings`, `GET /v1/meetings/{id}` (scopes `meetings:write`/`meetings:read`).
- `GET /v1/meetings/{id}/notes`, `PATCH /v1/notes/{id}` (assign/close action items).
- `POST /v1/transcripts/search` (keyword+semantic; owned by `prd-12`) and `POST /v1/chat` (#360).
- `POST /v1/meetings/{id}/clips`, `GET /v1/clips/{id}`.
- `POST /v1/comments`, `GET /v1/{subject}/{id}/comments`; `POST /v1/shares`.
- `GET /v1/meetings/{id}/scorecard`.
- `POST /v1/meetings/{id}/crm-push` → uses `internal/delivery`-style destination abstraction for CRM connectors (#364).

Concrete shapes:
```jsonc
// Schedule a bot
POST /v1/meetings
{ "provider": "meet", "join_url": "https://meet.google.com/abc-defg-hij",
  "calendar_event_id": "evt_018f...", "bot_name": "Orpheus Notetaker",
  "record_video": false }
// → 202 { "id": "mtg_018f...", "status": "scheduled", "poll_url": "/v1/meetings/mtg_018f..." }

// Ask AI across the corpus (#360)
POST /v1/chat
{ "scope": { "meeting_ids": ["mtg_018f..."] } | { "all": true },
  "question": "What did we commit to on pricing?" }
// → { "answer": "...", "citations": [{ "meeting_id": "mtg_018f...", "segment_idx": 412, "start": 1287.4 }] }

// Cut a highlight (#361)
POST /v1/meetings/{id}/clips
{ "title": "Pricing objection", "ranges": [{ "start": 1280.0, "end": 1332.5 }], "captions": true }
```
All chat/edit/clip work is dispatched as jobs (`POST /v1/jobs` with `_processor`) so metering, caching, and webhooks come free. New event types on the outbox: `meeting.recorded`, `meeting.notes.ready`, `clip.ready`, `note.action_item.assigned`, `meeting.failed` — all **additive** so existing subscribers are unaffected. These fan out through the existing outbox→NATS→webhook path (`README.md` "Events") with HMAC-signed, replayable deliveries — a customer's own automation can subscribe to `meeting.notes.ready` and fetch the structured notes without polling.

### 4.6 User stories
- As a sales manager, I want every rep's calls auto-joined and scored against a discovery scorecard (#363) so I can coach without listening to each call.
- As a PM, I want to ask "what feature requests came up this quarter?" across all meetings and get cited answers (#360, #359).
- As a podcaster, I want to delete a rambling answer by deleting its text and remove all my "um"s, then re-render (#365, #366).
- As a support lead, I want the call summary + action items auto-written into the CRM contact (#364).

### 4.5 Where it runs
- Bots + TTS/overdub + heavy chat embedding on **Modal** (GPU where needed), shared-secret auth (`orpheus-modal-auth`), **scale-to-zero with `max_containers` caps and join-at-start-time launch** so cost is bounded and no container idles between meetings.
- Notes/scorecard/chat LLM calls via the existing `get_llm()` layer (`openai-compat` → `infra/modal/orpheus_llm.py` by default, no external key).
- Control plane + data model in the Go API + Postgres, RLS-scoped per tx.
- All Modal calls and LLM passes are metered jobs (`jobs.cost_usd`/`usage_rollup`) so GPU-seconds and tokens are attributed per org.

## 5. Rollout / milestones

Milestones are **ordering only, not scope-cutting** — the full feature set in §4 stays in scope. Each milestone is itself production-grade and shippable: complete error handling, RLS, metering, observability, and e2e verification (§6) apply to every milestone, not a later "hardening phase."

- **M1 — Corpus foundation.** `transcripts` store + `meeting.notes` processor over already-uploaded files; `GET /v1/meetings` for file-based "meetings". Production-grade: org-scoped + FORCE RLS, notes degrade gracefully on malformed transcript / LLM JSON-parse failure, metered per org, observable (notes-ready latency). Delivers value without bots. **M1 is independently shippable and production-grade.** (Depends on `prd-12` transcript store.)
- **M2 — Bot ingestion.** Meeting-bot service (Meet first — cleanest headless join, remaining providers follow additively), reusing the streaming relay; calendar connector. Production-grade: bounded-retry join with join/kick/crash fallbacks (partial transcript always persisted, `status=degraded`), enforced recording-notice + per-region consent config, Modal scale-to-zero + `max_containers` + join-at-start-time, bot join-success/latency metrics, GPU-seconds metered per org.
- **M3 — Intelligence & collaboration.** Chat-over-transcripts (#360), scorecards (#363), comments/sharing (#362), highlight clips (#361). Production-grade: citation-validity enforced (retrieval-only fallback on unverifiable citations), RLS on comments/shares/clips, embedding-index-lag and citation-validity metrics, LLM tokens + clip-render GPU metered per org.
- **M4 — Editing & synthesis.** `edit.by_text` (#365), `edit.remove_fillers` (#366), then `tts.overdub`/dubbing (#367) behind consent gating. CRM auto-fill (#364) alongside M3/M4 per connector demand. Production-grade: non-destructive EDL with byte-reproducible render, overdub/voice-clone hard-gated on stored consent records, Modal TTS service scale-to-zero + `max_containers`, filler-removal accuracy measured e2e, TTS GPU-seconds metered per org.

## 6. Verification / acceptance criteria

All acceptance tests are **end-to-end against a real worker + Modal** (not unit-only), run in CI against a live meeting fixture and a live Modal deployment. Each is a production gate with a measurable target.

**Happy-path e2e (measured targets):**
- A scheduled Meet call is auto-joined, recorded, and **within N minutes of call-end** (target: notes-ready latency ≤ N min, measured call-end → `meeting.notes.ready` outbox event) produces a `meetings` row with diarized transcript, ≥1 summary note, action items, and decisions — **no upload API call made** (assert the meeting exists with zero `POST /v1/jobs` upload from the client).
- Cross-meeting search returns results ranked by both keyword and semantic relevance; `POST /v1/chat` answers cite `meeting_id`+`segment.start` that actually contain the claim — **≥95% citation validity measured e2e** over a labeled fixture set (each cited segment is programmatically checked to contain the claim; run fails below 95%).
- `edit.remove_fillers` on a fixture with known filler spans **removes ≥90% of them with zero non-filler cuts** (measured e2e against ground-truth filler spans; any non-filler cut fails the gate); render is byte-reproducible from the EDL.
- All new processors appear in `GET /v1/processors` with a pinned `(version, model_version_id)`; identical-input jobs hit the content cache (assert cache hit on repeat).
- Overdub/voice-clone **refuses to run without a stored consent record** for the target voice (assert `403`/rejected job with no audio synthesized).

**Negative / failure-path e2e (production gates):**
- **Bot kicked mid-call** → the streamed audio is finalized, a **partial transcript is still persisted**, `meetings.status=degraded` with `degraded_reason=bot_disconnected`, and `meeting.notes` still runs over the partial transcript (assert transcript non-empty + meeting queryable + degraded flag set).
- **Modal down / TTS/embedding/transcribe unavailable** → the affected job is **retried then fails cleanly** (job `status=failed`, no partial/corrupt artifact, meeting not lost — degraded not deleted), fallback backend engaged where one exists; assert no unhandled worker crash.
- **Malformed / truncated transcript** → `meeting.notes` **degrades gracefully** to a plain-text summary (JSON repair-retry then plain fallback), never hard-fails; `chat.answer` returns `citations_unverified` rather than fabricated citations.
- **Bot join blocked / recording disallowed** → `status=blocked`, `meeting.failed{reason=recording_blocked}` emitted, **no audio stored**.

**Multi-tenant isolation (production gate):**
- Every new table has a passing RLS isolation test in the style of `internal/db/db_rls_test.go` — **org A cannot read org B's meetings / notes / comments / transcripts / clips** (per-table assertions; treated as a release-blocking production gate, not a smoke test).

## 7. Dependencies, risks, open questions

- **Depends on** `prd-12` (transcript store, embedding index, retention/region) and `prd-14` (TTS/dubbing model artifacts; STS translation for dubbing; alignment for accurate word-level edits).
- **Legal/consent (high).** Recording bots and voice cloning are jurisdiction-sensitive; needs enforced recording-notice, per-region consent config, and consent records before cloning. Ties to data-residency in `prd-12`.
- **Platform ToS.** Zoom/Meet/Teams headless-join stability and ToS — prefer official bot/recording APIs where available; scrape-join as fallback.
- **Cost.** Standing bot containers are expensive; rely on Modal scale-to-zero + join-at-start-time scheduling.
- **Open:** Do we need per-participant audio tracks (better diarization) vs mixed track only? Which CRM first (HubSpot/Salesforce)? Is chat retrieval Postgres+pgvector or an external vector DB (decided in `prd-12`)?

## 8. Effort

- Transcript store + `meeting.notes` (M1): ~2–3 wk (shared with `prd-12`).
- Meeting-bot service + calendar + relay reuse (M2): ~5–7 wk (headless join is the long pole).
- Chat/scorecards/collaboration/clips (M3): ~4–6 wk.
- Editing + TTS/overdub/dubbing (M4): ~6–8 wk (new Modal TTS service + EDL render).

Total order-of-magnitude: **a quarter+** across the four milestones; **M1 is independently shippable and production-grade** and de-risks the rest.
