# Orpheus — Features & Issues (Checklist)

> **Compiled:** 2026-08-11 · **Checklist updated:** 2026-08-14 · Sibling to [`docs/COMPETITIVE_ANALYSIS.md`](docs/COMPETITIVE_ANALYSIS.md). PRDs for every unchecked item live in [`docs/prd/`](docs/prd/).
> **Method:** source-cited codebase audit + market research across API vendors (Deepgram, AssemblyAI, OpenAI, Gladia, Speechmatics, Rev, ElevenLabs, Google/AWS/Azure), dictation apps (Wispr Flow, Superwhisper, VoiceInk, Talon…), meeting SaaS (Otter, Fireflies, Descript, tl;dv, Fathom, Grain…), OSS (Whisper family, NeMo Parakeet/Canary, FunASR/SenseVoice, Kyutai/Moshi, Voxtral, MMS…), and realtime-voice stacks (Soniox, Cartesia, Deepgram Flux, Krisp, LiveKit, Pipecat, Vapi, Retell).

**Legend:** `- [x]` shipped · `- [ ]` not done · **🟡 partial** = some of it shipped, rest tracked. Closed GitHub issues are the source of truth; PR/issue refs are inline. **Releases:** v0.1.0 (stabilization) · v0.2.0 (real GPU audio-intelligence + streaming rewrite) · further intelligence processors on `main` since.

---

# PART A — Issues

## A1. ASR core / transcription quality
- [x] Default model configurable (was `tiny.en`-only); env + per-request `model` — #386, #396 · PR #458
- [x] Language **auto-detection** (was hardcoded `en`) — #387, #393 · PR #458
- [x] **GPU / `compute_type`** config (int8 default on CPU; float16 on GPU via Modal) — #388, #418 · PR #458, #461
- [x] **Modern model tier** (large-v3-turbo on GPU) — #389 · PR #461
- [x] **Custom vocabulary / keyterm biasing** (`initial_prompt`/`vocabulary`) — #391 · PR #458
- [x] **Cold-start** mitigation (warmup + upload-signal prewarm) — #394, #480 · PR #458, #481
- [ ] **Forced-aligned word timestamps** (WhisperX/wav2vec2) — 🟡 partial: word timestamps exist (DTW), not forced-aligned — #298
- [ ] **Inverse text normalization (ITN) / smart formatting** config — #392-adjacent
- [ ] **VAD-segmented long-file chunking** (batch still fixed 60 s windows) — #395
- [ ] **Code-switching** mid-utterance (auto-detect yes; single-language per pass)

## A2. Streaming
- [x] **O(n²) window re-transcription → LocalAgreement-2** incremental decoding — #397, #398 · PR #482
- [x] **VAD endpointing** (energy VAD flushes at pauses) — #399 · PR #482
- [x] **Server-side billing** (metered from received PCM; unspoofable) — #400 · PR #464
- [x] Clean `1000` WebSocket close (was abnormal `1006`) — #404 · PR #482
- [ ] **Semantic turn detection** (Smart-Turn-class model) — 🟡 partial: energy VAD only — #308
- [ ] **Realtime diarization** in the stream — #307
- [ ] **Sub-second partials + interim confidence** — 🟡 partial: partials yes, no confidence scores — #306, #310
- [ ] Streaming server started by `make dev` (still a separate process)

## A3. Audio intelligence
- [x] **Real diarization** (ECAPA embeddings + clustering on GPU; honest manifest) — #300, #405 · PR #468, #479
- [x] **Summarize / translate** real (Modal LLM, no external key) — #314, #315 · PR #478
- [x] **detect-language** real (Whisper detection + LLM fallback; honest) — #406, #407 · PR #468
- [x] **LLM-over-transcript** (summarize/translate + Q&A-capable) — #319 · PR #478
- [x] **Sentiment analysis** (`text.sentiment`) — #316 · PR #486
- [x] **Topic detection + key phrases** (`text.topics`) — #317 · PR #486
- [x] **Entity detection** (`text.entities`) — #318 · PR #486
- [x] **Speaker embeddings/ID** (ECAPA) — #320 · PR #479
- [ ] **PII redaction upgrade** — 🟡 partial: regex default; ML-NER (Presidio) opt-in — #A3-pii
- [ ] **Emotion recognition** (SenseVoice/SpeechBrain/Qwen-Audio) — #368
- [ ] **Audio-event detection** (laughter/music/applause) — #369
- [ ] **Auto-chapters** as first-class output — 🟡 partial: `summarize` has a `chapters` mode
- [ ] **Speaker enrollment** (persistent voiceprint across sessions)

## A4. Cost / billing / metering
- [x] **GPU-seconds metering** (real cost = gpu_seconds × rate) — #413 · PR #465
- [x] **Streaming metered server-side** (was client-supplied) — #400 · PR #464
- [x] **Hard budget-cap enforcement** (402 on over-limit) — #415, #430 · PR #467
- [ ] **LLM token-cost pass-through** for summarize/translate/analysis
- [ ] **Cache "savings"** computed on real GPU cost (not flat)
- [ ] **Billing (Dodo) coupled to real usage metering**

## A5. Infrastructure / scaling / performance
- [x] **GPU execution** (Modal services: transcribe/LLM/diarize) — #418 · PR #461
- [x] **Warm pools / prewarm** available (`min_containers`, scaledown, upload-prewarm) — #480 · PR #481
- [ ] **Inference batching** (dynamic/continuous GPU batching) — #420
- [ ] **In-app autoscaling** (consume the JetStream queue-depth gauge) — #421
- [ ] **Dynamic concurrency** (static `worker_concurrency`/`per_org_concurrency`)
- [ ] **Model registry wired to runtime loading** (S3/sha256) — #423
- [ ] **Multi-model routing / per-tier engine** — 🟡 partial: per-request model + Modal backends

## A6. Reliability / correctness
- [x] Prior stabilization bugs (API-key prefix collision, webhook ListDeliveries, test-fire body, redirect loop, pagination 404, null-safety, upload edges)
- [x] **Cache wrong-transcript collision** (empty sha256) — #459 · PR #460
- [x] **slice** `.bin` ffmpeg-muxer failure — #471 · PR #472
- [x] **Empty lists return `[]`** not `null` — #473 · PR #474
- [x] Streaming unit coverage (LocalAgreement/trim/VAD) — PR #482
- [ ] Integration test for the streaming **relay** path + ListDeliveries cursor edges
- [ ] Load/soak test of the GPU path

## A7. Security / auth / compliance
- [x] **Hard spend guardrail** (budget hard-cap blocks jobs) — #430 · PR #467
- [ ] **Keycloak JWT functional** (org_id claim mapper + platform:admin role) — #427
- [ ] **Real IdP** for the dashboard (replace dev-grade SQLite accounts) — #428
- [ ] **Rate-limiter fail-open** policy review (Redis outage → open)
- [ ] **SOC2 / HIPAA / BAA, SSO/SCIM, audit program** — #431
- [ ] **Streaming WS `CheckOrigin`** tightened for prod
- [ ] **Secrets management** beyond env vars
- [ ] **Marketplace sandbox** for third-party processor code — #432

## A8. Data / storage / lifecycle
- [ ] **Transcript store / search / retrieval** surface — #433
- [ ] **Semantic search / knowledge base** over transcripts — #434
- [ ] **Data residency / region selection** — #435
- [ ] **Retention policies / per-tenant TTLs**
- [ ] **Streaming/transcoded artifact delivery** (signed-URL only today)

## A9. Developer experience / API
- [x] **List-envelope standardized** (`{data, has_more, next_cursor}`) — #442 · PR #469
- [ ] **Official client SDKs** (Python/JS/Go) — #444
- [ ] **OpenAI/Deepgram-compatible endpoint** — #445
- [ ] **Job-create callback URL** + upload-and-poll helper
- [ ] **MCP server** for transcript retrieval / agents — #447
- [ ] **Processor SDK** for third parties

## A10. Observability / ops
- [ ] **Per-model latency/throughput + GPU-utilization metrics** — 🟡 partial: `gpu_seconds` returned, no dashboard
- [ ] **Cost dashboards** tied to real spend
- [ ] **SLA / uptime** commitment
- [ ] **Autoscaling signal consumed**

## A11. Product / UX gaps
- [ ] **Meeting bot / auto-join + live notes** — #454
- [ ] **Action items / decisions / highlights** — 🟡 partial: chapters via summarize
- [ ] **Noise suppression / echo / voice isolation** (Krisp-class)
- [ ] **Audio-edit-by-text** (Descript-style)
- [ ] **TTS / dubbing / voice cloning**
- [ ] **Collaboration** (comments, clips, sharing)
- [ ] **CRM / conversation-intelligence**
- [ ] **Dictation / voice-typing client + AI cleanup**
- [ ] **Caption styling / burn-in**

---

# PART B — Features (superset, priority-ordered)

Each item: **feature** (issue#) — *who ships it*. Checked = shipped in Orpheus.

## P0 — Core parity
- [x] Multilingual transcription (#294) — *everyone*
- [x] Modern model tier w/ published accuracy (#295) — *Nova-3, Universal-3, Parakeet, large-v3-turbo*
- [x] GPU inference (#296) — *all*
- [x] Auto language detection (#297) — *all*
- [x] Real speaker diarization (#300) — *pyannote, NeMo Sortformer*
- [x] Async batch + webhook callbacks (#301) — *all*
- [x] Real cost metering + hard budget caps (#302) — *AWS, OpenAI*
- [x] Subtitles/captions export (SRT/VTT) (#304) — *all*
- [x] Custom vocabulary / keyterm biasing (#305) — *all APIs*
- [ ] Accurate word-level timestamps (**forced alignment**) (#298) — 🟡 partial (DTW) — *WhisperX, Parakeet, NeMo NFA*
- [ ] Punctuation + smart formatting + **ITN** (#299) — 🟡 partial — *all*
- [ ] Multi-language **client SDKs** (#303) — *all*

## P1 — Competitive

**Realtime / streaming**
- [x] Model tiers (fast/balanced/accurate) selectable (#311) — *Deepgram, Cartesia, OpenAI*
- [ ] True streaming ASR, sub-300 ms partials (#306) — 🟡 partial (LocalAgreement, not <300 ms) — *Deepgram, AssemblyAI, Gladia, Soniox*
- [ ] Realtime diarization + word-level speaker attribution (#307) — *AssemblyAI, Deepgram, Speechmatics*
- [ ] VAD endpointing + **semantic turn detection** (#308) — 🟡 partial (VAD only) — *Deepgram Flux, Pipecat, LiveKit*
- [ ] Eager/speculative end-of-turn with resume (#309) — *Deepgram Flux, Cartesia*
- [ ] Interim confidence + word timestamps in stream (#310) — *Soniox, AssemblyAI*
- [ ] Realtime PII redaction (text) (#312) — *Deepgram, Speechmatics, AWS*

**Audio intelligence**
- [x] Summarization (#314) — *AssemblyAI, Gladia, Voxtral*
- [x] Translation (speech→text) (#315) — *Gladia, Speechmatics, Seamless*
- [x] Sentiment analysis (#316) — *AssemblyAI, Gladia, SenseVoice*
- [x] Topic detection / key phrases (#317) — *AssemblyAI, Gladia*
- [x] Entity detection (#318) — *AssemblyAI, Deepgram*
- [x] LLM-over-transcript (Q&A / custom summaries / RAG) (#319) — *AssemblyAI LeMUR, Otter*
- [x] Speaker identification / enrollment (embeddings) (#320) — 🟡 enrollment pending — *Speechmatics, SpeechBrain*
- [ ] PII redaction (text + audio "beep") (#313) — 🟡 partial (regex text) — *AssemblyAI, AWS, Deepgram*
- [ ] Code-switching mid-utterance (#321) — *Soniox, Deepgram, Gladia*
- [ ] Multichannel / stereo per-channel (#322) — *Gladia, Speechmatics, AWS*
- [ ] Profanity filter / content moderation (#323) — *Deepgram, AssemblyAI, AWS*

**Platform / infra / enterprise**
- [x] Async callbacks + webhooks (#324) — *all*
- [ ] Inference batching (throughput) (#325) — *vLLM, TensorRT-LLM, WhisperLive*
- [ ] Autoscaling on load (#326) — 🟡 partial (Modal-side) — *hyperscalers*
- [ ] OpenAI/Deepgram-compatible API (#327) — *Speaches, owhisper, vLLM*
- [ ] HIPAA/SOC2/BAA, SSO/SCIM, audit (#328) — 🟡 partial (audit+erasure) — *enterprise*
- [ ] Data residency / region selection (#329) — *AssemblyAI, ElevenLabs*
- [ ] MCP server for transcript retrieval / agents (#330) — *Wispr, Otter, Fathom*
- [ ] Custom model training / adaptation (BYO) (#335) — *AWS, Azure, Speechmatics*

## P2 — Advanced / differentiation

**Differentiators Orpheus holds**
- [x] Multi-tenant RLS SaaS isolation (#331) ⭐
- [x] Composable processor pipelines / workflows (#332) ⭐
- [x] Transparent per-second, GPU-metered pricing (#333) ⭐
- [x] Content-addressed result cache (#336) ⭐
- [x] GDPR erasure saga (#337)
- [ ] Self-host / on-prem / air-gapped (#334) — 🟡 partial (posture) ⭐ — *Speechmatics, Rev, Azure*
- [ ] Processor marketplace w/ **sandboxed 3rd-party code** (#432) — 🟡 partial (metadata-only) ⭐

**Dictation "flow" layer**
- [ ] LLM **cleanup pass** (filler removal, raw+clean dual output) (#338) — *Wispr Flow, Superwhisper*
- [ ] **Backtrack / self-correction** (#339) — *Wispr Flow*
- [ ] **Command / transform** endpoint (#340) — *Wispr Command Mode, VoiceInk*
- [ ] **Context-conditioned output** (#341) — *Wispr, Willow, VoiceInk*
- [ ] Style/tone modes as presets (#342) — 🟡 partial (composable jobs) — *Wispr, Superwhisper*
- [ ] Romanized output (Hinglish) (#343) — *Wispr*
- [ ] Sub-700 ms "flow" latency (#344) — *Wispr*

**Audio enhancement (Krisp-class)**
- [ ] AI noise suppression (#345) — *Krisp, LiveKit*
- [ ] Background Voice Cancellation (#346) — *Krisp BVC, LiveKit*
- [ ] Echo cancellation / de-reverb (#347) — *Krisp*
- [ ] Voice isolation (#348) — *Krisp*
- [ ] Accent conversion (#349) — *Krisp*
- [ ] Telephony-optimized (8 kHz) denoise (#350) — *LiveKit, Krisp*

**Voice-agent / conversational infra**
- [ ] Barge-in / interruption handling (#351) — *Vapi, LiveKit, Retell*
- [ ] Backchannel detection (#352) — *Vapi, Retell*
- [ ] Active-listening / addressed-only mode (#353) — *Speechmatics Flow*
- [ ] Voicemail detection (#354) — *Vapi, Retell*
- [ ] Call transfer / DTMF / SIP-RTP-WebRTC ingestion (#355) — *Vapi, Retell, LiveKit*
- [ ] Full-duplex / speech-to-speech (#356) — *Kyutai Moshi, Seamless*

**Meeting & media-intelligence**
- [ ] Meeting bot / auto-join (#357) — *Otter, Fireflies, tl;dv*
- [ ] Live notes + action items + decisions (#358) — *meeting tools*
- [ ] Cross-meeting / semantic search + KB (#359) — *Otter, Fireflies, Fathom*
- [ ] Ask-AI / chat over transcript(s) (#360) — *Otter, Fireflies, Fathom*
- [ ] Highlight reels / clips / soundbites (#361) — *Grain*
- [ ] Collaboration (comments, sharing) (#362) — *meeting tools*
- [ ] Conversation intelligence (scorecards) (#363) — *Grain, tl;dv, Gong*
- [ ] CRM auto-fill / field sync (#364) — *tl;dv, Fathom*
- [ ] Audio-edit-by-text (#365) — *Descript*
- [ ] Filler-word removal as edit / multitrack (#366) — *Descript*
- [ ] Overdub / voice clone / TTS / dubbing (#367) — *Descript, ElevenLabs*

**Model / deployment differentiators**
- [ ] Emotion recognition (#368) — *SenseVoice, SpeechBrain, Qwen-Audio*
- [ ] Audio-event detection (#369) — *SenseVoice, sherpa-onnx*
- [ ] Speech-to-speech translation (#370) — *Soniox, Seamless*
- [ ] On-device / edge model artifacts (#371) — *whisper.cpp, WhisperKit, Moonshine*
- [ ] 1000+ languages (#372) — *Meta MMS*
- [ ] Human transcription tier (#373) — *Rev.com*
- [ ] Forced alignment to external reference text (#374) — *WhisperX, NeMo NFA*
- [ ] Zero-data-retention / privacy mode (#375) — 🟡 partial (erasure) — *Wispr, Willow*
- [ ] Custom AI prompt templates / named modes (#376) — 🟡 partial (composable jobs) — *Superwhisper*

## P3 — Niche / frontier
- [ ] Realtime speaker enrollment / voiceprint ID (#377) — white space
- [ ] Realtime audio-event detection (#378) — white space
- [ ] Realtime PII redaction of the audio itself (#379) — frontier
- [ ] Realtime emotion / acoustic scene / age-gender (#380) — niche
- [ ] Non-speech noise input / OS voice-control overlays (#381) — *Talon* (app UX)
- [ ] Programmable command grammars + scripting (#382) — *Talon, Dragon* (app UX)
- [ ] Ambient/background sound injection for agents (#383) — *Vapi*
- [ ] AI QA auto-scoring of calls (#384) — *Retell*
- [ ] Watched-folders / URL ingest / per-app modes (#385) — dictation apps (app UX)

---

## Notes
- **Platform vs app.** Dictation/meeting client-UX items (cursor injection, hotkeys, menu-bar HUD, OS voice-control) are end-user app concerns; the platform-ownable slices are model quality, the LLM cleanup/command layer, formatting, languages, diarization/timestamps, audio enhancement, and compliance posture.
- **PRDs.** Every unchecked (and 🟡 partial) item has an implementation PRD in [`docs/prd/`](docs/prd/), grouped by epic. Issue numbers above may be approximate for a few later rows — filter the repo's `type:feature`/`type:issue` labels for the canonical ticket.
- **Verification.** Competitor claims are vendor-published; pricing/latency drift — re-verify before quoting. See `docs/COMPETITIVE_ANALYSIS.md` for NOT-VERIFIED flags.
