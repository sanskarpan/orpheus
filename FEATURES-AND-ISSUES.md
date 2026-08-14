# Orpheus — Features & Issues

> **Compiled:** 2026-08-11 · Living planning doc. Sibling to [`docs/COMPETITIVE_ANALYSIS.md`](docs/COMPETITIVE_ANALYSIS.md).
> **Method:** source-cited codebase audit + 10 web-research agents across API vendors (Deepgram, AssemblyAI, OpenAI, Gladia, Speechmatics, Rev, ElevenLabs, Google/AWS/Azure), the dictation category (Wispr Flow, Superwhisper, VoiceInk, Talon…), meeting-intelligence SaaS (Otter, Fireflies, Descript, tl;dv, Fathom, Grain…), the open-source ecosystem (Whisper family, NeMo Parakeet/Canary, FunASR/SenseVoice, Kyutai/Moshi, Voxtral, MMS, self-host servers…), and realtime-voice stacks (Soniox, Cartesia, Deepgram Flux, Krisp, LiveKit, Pipecat, Vapi, Retell).
> **Two lists below:** (A) **every issue** that needs fixing — no priority, exhaustive. (B) **every feature** anyone in the market ships — priority-ordered but complete ("if anyone has it, we want it on the list"). Orpheus status tags: `[HAVE]` `[PARTIAL]` `[STUB]` (real interface, placeholder impl) `[MISSING]`.

> **⚠️ Status note (updated 2026-08-12):** Much of Part A is now **fixed** and much of Part B **P0/P1 shipped** across releases **v0.1.0** and **v0.2.0**. The per-item `[HAVE]/[PARTIAL]/[STUB]/[MISSING]` tags in the tables below reflect the *original* audit; the Progress section immediately below is the current state, and **closed GitHub issues are the source of truth**.

---

## ✅ Progress — shipped since the audit (v0.1.0 → v0.2.0)

**Issues fixed (Part A):**
- **ASR core:** model/device/`compute_type` configurable (int8 default), **auto language detection** (was English-only), **custom vocabulary**, **warmup**, per-request model — #386–388, #391, #393, #394, #396 · PR #458.
- **Cache wrong-transcript collision** (empty `sha256` inputHash returned another input's transcript) — #459 · PR #460.
- **Streaming:** server-side billing (unspoofable) #400, clean `1000` close #404, **O(n²) window → LocalAgreement-2 + VAD endpointing** #397–399, #402 · PR #464, #482.
- **Cost/budget:** GPU-seconds metering #413, **hard-cap enforcement (402)** #415/#430, fractional-limit PATCH #466 · PR #465, #467.
- **Honest manifests:** diarize/summarize/translate no longer advertise real models while stubbed #405–407 · PR #468.
- **List envelopes** uniform + `[]` (not `null`) when empty — #442, #473 · PR #469, #474.
- **slice** `.bin` ffmpeg-muxer failure on uploaded artifacts — #471 · PR #472.
- **GPU execution** real via Modal — #418 · PR #461.

**Features shipped (Part B):**
- **P0:** GPU inference #296 · modern model tier (large-v3-turbo) #295 · **multilingual + auto-langid** #294/#297 · **custom vocab** #305 · **real cost metering + hard caps** #302 · **model tiers** #311.
- **P1:** **real diarization** (ECAPA on GPU) #300 · **speaker embeddings/ID** #320 · **summarization** #314 · **translation** #315 · **LLM-over-transcript** #319 · **provider-agnostic LLM** (anthropic/openai/gemini/openai-compat) #476 · **transparent GPU-metered pricing** #333.
- **P2/infra:** **cold-start prewarming** #480 · pre-existing HAVEs confirmed & closed — RLS SaaS #331, composable pipelines #332, content cache #336, GDPR erasure #337, async+webhooks #301, captions #304.
- **Modal GPU services** (`infra/modal/`): transcribe (large-v3-turbo), open LLM (Qwen2.5-3B via vLLM, no external key), diarization (ECAPA) — all authenticated, scale-to-zero.

**Post-v0.2.0 (main):**
- **Audio-intelligence processors** — **sentiment** #316 (`text.sentiment`), **topics + key phrases** #317 (`text.topics`), **entity detection** #318 (`text.entities`) — LLM-backed, real on the Modal LLM · PR #486.

**Still PARTIAL / open (representative):** VAD yes but no **semantic turn detection** #308 · sub-300 ms streaming #306 · interim **confidence** #310 · **forced alignment** (word timestamps only) #298 · realtime diarization #307 · client **SDKs**, **MCP server**, **OpenAI-compatible endpoint**, meeting-intelligence, audio-enhancement (Krisp-class), on-device — see open `type:feature` issues.

Releases: **v0.1.0** (stabilization) · **v0.2.0** (real GPU audio-intelligence + streaming rewrite); further intelligence processors on `main` since.

---

# PART A — Issues (exhaustive, unprioritized)

Everything currently wrong, incomplete, coarse, stubbed, or missing. Grouped by area; `file:line` where it points at code. No priority ordering — fix-planning happens elsewhere.

## A1. ASR core / transcription quality
- Default transcription model is **`tiny.en`** — the smallest, least-accurate Whisper; high WER. `transcribe.py:33`, `processors/transcribe.py:84,156`
- **`language="en"` is hardcoded** → effectively English-only despite Whisper being multilingual and returning a detected language. `transcribe.py:47–52`
- **No GPU / no compute-type config** — `WhisperModel` created without `device=`/`compute_type=` → CPU defaults, no int8, no CUDA. `transcribe.py:17–28`
- **No modern model option** — no large-v3-turbo / distil-whisper / Parakeet / Canary path; no speed/accuracy tiers.
- **Word timestamps are DTW/cross-attention (~±500 ms)**, not forced-aligned (no WhisperX/wav2vec2) → imprecise. `transcribe.py:56–66`
- **No custom vocabulary / keyterm biasing** at the recognition layer.
- **No inverse text normalization (ITN)** / configurable smart formatting (numerals, dates, currency, addresses).
- **No auto language detection exposed** and **no code-switching** support (forced-en blocks both).
- **Cold start** — first job pays full model-load latency (lazy module singleton); no warm pool. `transcribe.py:10–28`
- **Long-file chunking is fixed 60 s windows**, not VAD-segmented → cuts across speech, wastes compute on silence. `processors/transcribe.py:19,86–135`
- Single model singleton per worker → **no per-request/per-tier model selection at runtime**.

## A2. Streaming
- **Window re-transcription**: each partial re-runs whole-file Whisper over the entire growing 3 s window → **O(n²) recompute**, latency/cost grow with window length. `streaming.py:91–161`
- **No true incremental / streaming decoding**; no cache-aware streaming model.
- **No VAD endpointing / semantic turn detection** — no interim-vs-final logic beyond the window timer.
- **Streaming billing duration is client-reported at finalize** → inaccurate and trivially spoofable. `streaming.go:157`
- **No realtime diarization.**
- **No sub-second partials, no interim confidence.**
- Streaming ASR server is a **separate process not started by `make dev`** and its dependency (worker WS on :8082) is easy to miss operationally.
- Relay closes with an **abnormal 1006** (no close frame) on client disconnect — cosmetic but noisy. `streaming_ws.go`

## A3. Audio intelligence (stubs & absences)
- **Diarization is a round-robin stub by default** (speakers by 5 s window), and the **manifest advertises `model_id="pyannote"` regardless** — misleading. `diarize.py:32–94`, `audio_ops.py:119–128`
- **Summarize / translate echo a placeholder** unless `ANTHROPIC_API_KEY` is set. `llm.py:40–138`, `text_ops.py:90–170`
- **detect-language falls back to a heuristic stub.** `text_ops.py:59–87`
- **PII is regex-only by default**; ML-NER (Presidio) requires an extra + env flag. `redact.py:56–111`
- **No sentiment / topic / intent / emotion / audio-event detection** anywhere.
- **No LLM-over-transcript** (Q&A, chapters, custom summaries, RAG).
- **No speaker identification / enrollment** (voiceprint across sessions).
- **No entity detection, no key-phrase extraction, no auto-chapters.**

## A4. Cost / billing / metering
- **Flat cost constant** — `cost = duration × $0.00005` for every processor regardless of real work; overwrites the per-job flat rate at completion. `config.py:20–22`, `worker.py:248`
- **No GPU-seconds metering**; **no per-model cost**.
- **No LLM token-cost pass-through** for summarize/translate.
- **Streaming cost = client-supplied `audio_seconds`** — unmetered, spoofable. `streaming.go:29,157`
- **Budgets are advisory-only** — a polling loop fires threshold alerts; **job creation never consults budgets; nothing caps spend.** `usage/service.go:113–171`
- **Cache "savings" estimate** uses the source job's flat cost, not real GPU cost. `cache.go:98–131`
- Billing (Dodo) exists but is **decoupled from real usage metering**.

## A5. Infrastructure / scaling / performance
- **No GPU execution anywhere**; GPU tier enum exists but **no processor declares one**. `processors/__init__.py`
- **No inference batching** — the `batching` package aggregates job *results*, not GPU inputs; no dynamic/continuous batching. `batching/service.go`
- **No in-app autoscaling** — a JetStream queue-depth gauge is exported but nothing consumes it. `worker.py:52–65`
- **Static concurrency** — `worker_concurrency=4`, `per_org_concurrency=8`. `config.py:16–25`
- **No warm model pools / no serverless-GPU integration.**
- **Model registry (S3, sha256-verified) is not wired into runtime model loading** — processors still load Whisper via env/HF path. `model_registry.py:42–129`
- No multi-model routing / no per-tier engine selection.

## A6. Reliability / correctness
- Fixed this session (recorded for history): API-key **prefix-collision 401s**; webhook **`ListDeliveries` param off-by-one** (deliveries never listed); webhook **test-fire missing body 400**; web **redirect loop on stale cookie**, missing **error boundaries**, **jobs pagination 404**, transcribe **poller give-up**, **null-safety** gaps, **upload edge cases**. See `apps/web/issues.md`.
- **No integration test** for the streaming relay path or the `ListDeliveries` cursor/status edges.
- No load/soak test of the GPU path (because there is no GPU path yet).

## A7. Security / auth / compliance
- **Keycloak JWT is non-functional in practice** — no `org_id` claim mapper and no `platform:admin` realm role → prod rejects tokens; non-prod collapses to a default org. `keycloak/realm-orpheus.json`, `keycloak.go:113–119`
- Web dashboard auth is **local SQLite accounts (dev-grade)**, not a real IdP; first-admin bootstrap is manual (`make bootstrap-admin`).
- **Rate limiter fails OPEN** on Redis error (availability over safety — a deliberate but flag-worthy choice). `ratelimit/limiter.go`
- **No hard spend guardrail** → cost-DoS risk (a tenant can run unbounded jobs).
- **Not SOC2/HIPAA-certified**; audit log + GDPR erasure exist but no formal compliance program, SSO/SCIM, or data-residency controls.
- Streaming WS uses a 2-min HMAC token with **`CheckOrigin` allow-all** — fine for dev, must tighten for prod. `streaming_ws.go`
- No secrets-management story beyond environment variables.
- Marketplace is metadata-only today; **any future third-party code execution needs a real sandbox** (currently absent).

## A8. Data / storage / lifecycle
- **No transcript store / search / retrieval product surface** — job results exist but there's no index or search.
- **No semantic search** over transcripts; no knowledge base.
- **No data-residency / region selection.**
- **No retention policies** beyond the erasure saga; no per-tenant TTLs.
- Artifact delivery is signed-URL only — no transcoding or streaming delivery.

## A9. Developer experience / API
- **No official client SDKs** (Python/JS/Go) — only raw HTTP.
- **No OpenAI-compatible or Deepgram-compatible endpoint** → adoption friction vs Speaches/owhisper/vLLM.
- **List-envelope inconsistency** — `{data,has_more,next_cursor}` vs `{data,has_more}` vs `{data}` across endpoints.
- **No job-create callback URL** (webhooks only); no simple upload-and-poll SDK helper.
- **No MCP server** for transcript retrieval / agent integration (a pattern Wispr/Otter/Fathom/Fireflies now ship).
- **No processor SDK** for third parties; marketplace can't accept real processors.

## A10. Observability / ops
- **No per-model latency/throughput metrics; no GPU-utilization metrics** (nothing to measure yet).
- **No cost dashboards tied to real spend.**
- **No SLA / uptime commitment.**
- Autoscaling signal exported but unused.

## A11. Product / UX gaps (surfaced by competitor comparison)
- No **meeting bot / auto-join** (Zoom/Meet/Teams); no live meeting notes.
- No **action items / decisions / chapters / highlights** extraction.
- No **noise suppression / echo cancellation / voice isolation / de-reverb** (Krisp-class).
- No **audio-edit-by-text** (Descript-style), filler-word-removal-as-edit, overdub/voice clone, multitrack.
- No **TTS / dubbing / voice cloning.**
- No **collaboration** (comments, clips/soundbites, sharing, highlight reels).
- No **CRM / conversation-intelligence** (talk-time, coaching, BANT/MEDDIC scorecards).
- No **dictation / voice-typing client** with an AI cleanup pass.
- No **caption styling / burn-in**; SRT/VTT builder only.

---

# PART B — Features (superset, priority-ordered, complete)

Every capability seen anywhere in the market. Ordered by priority tier; within a tier, grouped by category. Each row: the feature, **who ships it** (representative), and **Orpheus status**. Priority = "how badly do we need it to be credible," not "how good it is" — even niche items are listed (per the brief: _if anyone has it, list it_).

Legend: `[HAVE]` shipped · `[PARTIAL]` real-but-incomplete/add-on · `[STUB]` interface-only · `[MISSING]`.

## P0 — Core parity (credibility floor; a serious ASR platform is expected to have all of these)

| Feature | Who ships it | Orpheus |
|---|---|---|
| Multilingual transcription (90–100+ langs) | everyone | `[MISSING]` (en-only) |
| Modern model tier with published accuracy | Nova-3, Universal-3, Parakeet, Canary, large-v3-turbo | `[MISSING]` (tiny.en) |
| GPU inference | all APIs, all serving stacks | `[MISSING]` |
| Auto language detection | all | `[MISSING]` (forced en) |
| Accurate word-level timestamps (forced alignment) | WhisperX, Parakeet, NeMo NFA, all APIs | `[PARTIAL]` (DTW) |
| Punctuation + smart formatting + ITN (numerals/dates/currency) | all | `[PARTIAL]` |
| Real speaker diarization | pyannote, NeMo Sortformer, all APIs | `[STUB]` |
| Async batch transcription + webhook callbacks | all | `[HAVE]` |
| Real cost metering (GPU-seconds / tokens) + hard budget caps | AWS, OpenAI, cloud | `[PARTIAL]` (flat + advisory) |
| Multi-language client SDKs (Python/JS/Go) | all | `[MISSING]` |
| Subtitles/captions export (SRT/VTT) | all | `[HAVE]` |
| Custom vocabulary / keyterm biasing | all APIs | `[MISSING]` |

## P1 — Competitive (most serious players ship these; needed to win deals)

**Realtime / streaming**
| Feature | Who | Orpheus |
|---|---|---|
| True streaming ASR, sub-300 ms partials | Deepgram, AssemblyAI, Gladia (<103 ms), Soniox, ElevenLabs, Cartesia (66 ms) | `[PARTIAL]` (window) |
| Realtime diarization + word-level speaker attribution | AssemblyAI, Deepgram, Speechmatics, Soniox | `[MISSING]` |
| VAD endpointing + **semantic turn detection** | Deepgram Flux, Pipecat Smart-Turn, LiveKit, Soniox, Cartesia Ink 2 | `[MISSING]` |
| Eager/speculative end-of-turn with resume | Deepgram Flux, Cartesia | `[MISSING]` |
| Interim confidence + word timestamps in stream | Soniox, AssemblyAI, Deepgram | `[MISSING]` |
| Model tiers (fast / balanced / accurate) selectable | Deepgram, Cartesia, AssemblyAI, OpenAI | `[MISSING]` |
| Realtime PII redaction (text) | Deepgram, Speechmatics, AWS | `[MISSING]` |

**Audio intelligence**
| Feature | Who | Orpheus |
|---|---|---|
| PII redaction (text + audio "beep") | AssemblyAI, AWS, Deepgram, Gladia, Speechmatics | `[PARTIAL]` (regex text) |
| Summarization | AssemblyAI, Gladia, Speechmatics, AWS, Voxtral, Canary-Qwen | `[STUB]` |
| Translation (speech→text) | Gladia, Speechmatics, Seamless, Canary, Soniox, Whisper | `[STUB]` |
| Sentiment analysis | AssemblyAI, Gladia, Speechmatics, SenseVoice, SpeechBrain | `[MISSING]` |
| Topic detection / auto-chapters / key phrases | AssemblyAI, Gladia, meeting tools | `[MISSING]` |
| Entity detection | AssemblyAI, Deepgram | `[MISSING]` |
| LLM-over-transcript (Q&A / custom summaries / RAG) | AssemblyAI LeMUR, Voxtral, Canary-Qwen, Otter, Fireflies | `[MISSING]` |
| Speaker identification / enrollment (voiceprint) | Speechmatics, SpeechBrain, 3D-Speaker | `[MISSING]` |
| Code-switching mid-utterance | Soniox, Deepgram, Gladia | `[MISSING]` |
| Multichannel / stereo per-channel | Gladia, Speechmatics, AWS | `[MISSING]` |
| Profanity filter / content moderation | Deepgram, AssemblyAI, AWS | `[MISSING]` |

**Platform / infra / enterprise**
| Feature | Who | Orpheus |
|---|---|---|
| Inference batching (throughput) | vLLM, TensorRT-LLM, WhisperLive, faster-whisper batched | `[MISSING]` |
| Autoscaling on load | hyperscalers | `[MISSING]` |
| OpenAI-compatible / Deepgram-compatible API | Speaches, owhisper, vLLM | `[MISSING]` |
| Async callbacks + webhooks | all | `[HAVE]` |
| HIPAA / SOC2 / BAA, SSO/SCIM, audit logs | all enterprise vendors | `[PARTIAL]` (audit+erasure; not certified) |
| Data residency / region selection | AssemblyAI, ElevenLabs, hyperscalers | `[MISSING]` |
| MCP server for transcript retrieval / agents | Wispr, Otter, Fathom, Fireflies | `[MISSING]` |
| Custom model training / adaptation (BYO acoustic/LM) | AWS, Azure, Speechmatics | `[MISSING]` |

## P2 — Advanced / differentiation (fewer have these; several are Orpheus's wedge)

**Differentiators Orpheus uniquely holds or half-holds** (lean in):
| Feature | Who else | Orpheus |
|---|---|---|
| Multi-tenant RLS SaaS isolation | ~nobody (pure-API) | `[HAVE]` ⭐ |
| Composable processor pipelines / workflows | ~nobody as a product | `[HAVE]` ⭐ |
| Transparent per-second, GPU-metered pricing | (opaque per-hour elsewhere) | `[PARTIAL]` — needs real metering ⭐ |
| Self-host / on-prem / air-gapped | Speechmatics, Rev, Azure, Google | `[PARTIAL]` (posture) ⭐ |
| Processor marketplace w/ **sandboxed 3rd-party code** | nobody | `[PARTIAL]` (metadata-only) ⭐ |
| Content-addressed result cache | (internal to vendors) | `[HAVE]` ⭐ |
| GDPR erasure saga (verifiable) | Gladia, ElevenLabs | `[HAVE]` |

**The dictation "flow" layer** (Wispr Flow's moat — mostly model/infra work a platform can own):
| Feature | Who | Orpheus |
|---|---|---|
| LLM **cleanup pass** — filler removal, punctuation, adjustable intensity, **raw+clean dual output** | Wispr Flow, Superwhisper, VoiceInk | `[MISSING]` |
| **Backtrack / self-correction** ("2… actually 3", "scratch that") | Wispr Flow | `[MISSING]` |
| **Command / transform** endpoint (selected text + spoken instruction → rewrite) | Wispr Command Mode, VoiceInk | `[MISSING]` |
| **Context-conditioned output** (app/field/on-screen context + style/vocab profile) | Wispr, Willow (Style Memory), VoiceInk | `[MISSING]` |
| Style/tone modes (formal/casual/email/code) as pipeline presets | Wispr, Superwhisper Modes | `[PARTIAL]` (jobs are composable) |
| Romanized output (e.g. Hinglish) | Wispr | `[MISSING]` |
| Sub-700 ms end-to-end "flow" latency target | Wispr (<700 ms p99) | `[MISSING]` |

**Audio enhancement (Krisp-class):**
| Feature | Who | Orpheus |
|---|---|---|
| AI noise suppression (uplink/downlink) | Krisp, LiveKit | `[MISSING]` |
| Background Voice Cancellation (remove other speakers) | Krisp BVC, LiveKit | `[MISSING]` |
| Echo cancellation (AEC) / de-reverberation | Krisp | `[MISSING]` |
| Voice isolation | Krisp | `[MISSING]` |
| Accent conversion (realtime) | Krisp | `[MISSING]` |
| Telephony-optimized (8 kHz) denoise | LiveKit BVCTelephony, Krisp | `[MISSING]` |

**Voice-agent / conversational infra:**
| Feature | Who | Orpheus |
|---|---|---|
| Barge-in / interruption handling | Vapi, Speechmatics Flow, LiveKit, Pipecat, Retell | `[MISSING]` |
| Backchannel detection ("right", "okay") | Vapi, Retell | `[MISSING]` |
| Active-listening / addressed-only mode | Speechmatics Flow | `[MISSING]` |
| Voicemail detection (AMD/beep/LLM) | Vapi (5 methods), Retell | `[MISSING]` |
| Call transfer / warm handoff, DTMF, SIP/RTP/WebRTC ingestion | Vapi, Retell, LiveKit | `[MISSING]` |
| Full-duplex / speech-to-speech | Kyutai Moshi, Seamless-Streaming | `[MISSING]` (frontier) |

**Meeting & media-intelligence (product layer / building blocks):**
| Feature | Who | Orpheus |
|---|---|---|
| Meeting bot / auto-join (Zoom/Meet/Teams) | Otter, Fireflies, tl;dv, Fathom, Grain | `[MISSING]` |
| Live notes + action items + decisions | all meeting tools | `[MISSING]` |
| Cross-meeting / semantic search + knowledge base | Otter, Fireflies, Fathom | `[MISSING]` |
| Ask-AI / chat over transcript(s) | Otter AI Chat, Fireflies, Fathom (MCP) | `[MISSING]` |
| Highlight reels / clips / soundbites | Grain (Stories), meeting tools | `[MISSING]` |
| Collaboration (comments, sharing, timestamp links) | meeting tools | `[MISSING]` |
| Conversation intelligence (talk-time, coaching, BANT/MEDDIC scorecards) | Grain, tl;dv, Gong-class | `[MISSING]` |
| CRM auto-fill / field sync (HubSpot/Salesforce) | tl;dv, Fathom, Fireflies | `[MISSING]` |
| **Audio-edit-by-text** (edit transcript → edit media) | Descript | `[MISSING]` |
| Filler-word removal **as an edit**, multitrack | Descript | `[MISSING]` |
| Overdub / voice clone / TTS / dubbing | Descript, ElevenLabs | `[MISSING]` |

**Model / deployment differentiators:**
| Feature | Who | Orpheus |
|---|---|---|
| Emotion recognition | SenseVoice, SpeechBrain, Qwen-Audio | `[MISSING]` |
| Audio-event detection (laughter/music/applause) | SenseVoice, sherpa-onnx, Qwen-Audio | `[MISSING]` |
| Speech-to-speech translation | Soniox, Seamless | `[MISSING]` |
| On-device / edge model artifacts (quantized) | whisper.cpp, WhisperKit, Parakeet, Moonshine | `[MISSING]` |
| 1000+ languages | Meta MMS | `[MISSING]` (niche) |
| Human transcription tier | Rev.com | `[MISSING]` |
| Forced alignment to external reference text (subtitle sync) | WhisperX, NeMo NFA | `[MISSING]` |
| Zero-data-retention / no-training / privacy mode | Wispr, Willow, enterprise | `[PARTIAL]` (erasure; no ZDR toggle) |
| Custom AI prompt templates / named modes | Superwhisper, dictation, meeting tools | `[PARTIAL]` (composable jobs) |

## P3 — Niche / frontier (long-tail; some are genuine white space nobody ships)

- **Realtime speaker enrollment / voiceprint ID** — white space (async only elsewhere).
- **Realtime audio-event detection** (laughter/music) — white space (batch only elsewhere: SenseVoice).
- **Realtime PII redaction of the audio itself** (not just text) — frontier.
- **Realtime emotion / tone, acoustic scene, age/gender** — niche, mostly research/adjacent.
- Non-speech "noise" input (mouth pop/hiss), eye-tracking control, OS voice-control overlays — Talon (**end-user app UX, not a platform concern**).
- Programmable command grammars + scripting (`.talon` + Python), Dragon macros — dictation apps (app UX).
- Ambient/background sound injection for agents — Vapi.
- AI QA auto-scoring of calls — Retell.
- Watched-folders / YouTube-URL ingest, per-app auto-activation modes — dictation apps.

---

## Notes on scope & verification
- **Platform vs app.** Many dictation/meeting features are **end-user client UX** (system-wide cursor injection, hotkeys, menu-bar HUD, OS voice-control, dictation history UI, cross-device sync). Those are marked as app-UX in the research and are *not* Orpheus's concern — the platform-ownable slices are transcription/model quality, the LLM cleanup/command layer, formatting, languages, diarization/timestamps, audio enhancement, and the ZDR/compliance posture.
- **The strategic inversion.** Orpheus's `[HAVE]`s cluster in exactly the P2 differentiators the pure-API vendors lack (multi-tenant RLS, composable pipelines, self-host, marketplace, cache, GDPR); its `[MISSING]`s cluster in the P0/P1 ASR-quality, streaming, and audio-intelligence table stakes. The foundation work is: make the core competitive (P0/P1) so the wedge (P2) matters.
- **Verification.** Competitor claims are vendor-published (independent WER is largely unavailable); pricing/latency drift — re-verify before quoting. Items the research could not confirm are marked NOT VERIFIED in `docs/COMPETITIVE_ANALYSIS.md`. Orpheus claims cite `file:line` on `main` as of 2026-08-11.
