# Orpheus — Competitive Feature-Parity & Market Gap Analysis

> **Compiled:** 2026-08-11 · **Method:** 6 parallel web-research agents (official pricing/docs/benchmarks) + source-cited codebase audit.
> **Rendered version (interactive, private):** https://claude.ai/code/artifact/bc1743c8-421d-4af6-b19c-87eeb38a5901
> **Status:** strategic snapshot. Competitor pricing/latency drift — re-verify before quoting externally. Anything the research could not confirm is marked **NOT VERIFIED** rather than guessed.

---

## Thesis

**Orpheus is a strong _platform_ wrapped around a weak _ASR core_.** The plumbing — multi-tenant RLS, a JetStream job pipeline with retries/DLQ/outbox, transactional webhook delivery, a content-addressed result cache, and GDPR erasure — is genuinely production-grade and is _exactly where the pure-API vendors are blank_. But the engine is English-only `tiny.en` on **CPU**, "streaming" re-transcribes a growing 3-second window (an **O(n²)** anti-pattern), and diarization / summarize / translate / PII ship as **stubs by default**. The competitive gap is the model + performance + audio-intelligence layer, not the infrastructure.

## Executive summary (6 findings)

1. **The plumbing is real; the engine is a demo.** Production-grade infra; transcription is `tiny.en`/CPU/English-only; diarize/summarize/translate/PII default-stubbed.
2. **Everyone streams sub-300 ms; Orpheus re-transcribes windows.** The 3s window re-runs whole-file Whisper each partial → O(n²) recompute. Biggest latency/cost defect.
3. **Cost is an opening, not a liability — if metered honestly.** Hosted APIs $0.15–0.46/audio-hr; self-host batched 20–490× cheaper on paper; **Groq turbo $0.04/hr is the real floor**. Orpheus's flat CPU-second constant models none of this.
4. **The market moved to bundled audio intelligence** (AssemblyAI LeMUR, Gladia, AWS Call Analytics). Orpheus has interfaces, stubbed implementations.
5. **Four differentiators no pure-API player combines:** multi-tenant RLS SaaS + self-host/on-prem + transparent per-second pricing + processor marketplace.
6. **Priority order:** (a) kill the streaming window; (b) batch on GPU with a modern model (Parakeet-TDT / large-v3-turbo) + int8 + WhisperX; (c) make diarization/PII/summarize real; (d) meter real cost + enforce budgets.

---

## Part 1 — Ground truth: what Orpheus does today

Legend: **BUILT** = production-real · **PARTIAL** = real but incomplete/advisory · **STUB-DEFAULT** = real interface, placeholder impl unless external key/model configured · **MISSING**.

| Area | Status | Reality (file:line) |
|---|---|---|
| API surface | BUILT | ~20 `/v1` domains: uploads (resumable multipart + URL ingest), artifacts, jobs (+bulk/requeue), webhooks (11 routes), api-keys, usage/audit, budgets, billing, erasure, cache, batches, destinations, bundles, marketplace, onboarding, streaming. Authn→rate-limit→idempotency→audit. `server.go:172–316` |
| Transcription | PARTIAL | faster-whisper (CTranslate2), default **`tiny.en`**, **`language="en"` hardcoded** → effectively monolingual English. Long files chunked; word timestamps supported. **No `device=`/`compute_type=`** → CPU, no int8/GPU. `transcribe.py:33–66` |
| Streaming ASR | PARTIAL | Rolling-buffer **window re-transcription**: 3s windows, 1s partials, un-finalized tail fully re-transcribed each partial. API WS relay w/ HMAC token auth → worker WS. **Billing duration client-reported.** `streaming.py:91–161`, `streaming_ws.go:35–170` |
| Diarization | STUB-DEFAULT | **Round-robin labels by 5s window** unless `ORPHEUS_DIARIZE_MODEL`+HF token+`diarize` extra (then pyannote). Manifest claims `model_id="pyannote"` regardless. `diarize.py:32–94` |
| Summarize / Translate | STUB-DEFAULT | Echo/placeholder unless `ANTHROPIC_API_KEY` → real Claude (injection-sandboxed). `llm.py:40–138`, `text_ops.py:90–170` |
| PII redaction | PARTIAL | Real regex (email/phone/SSN/CC+Luhn/IP) always on; ML-NER (Presidio) only with `pii` extra + engine flag. Un-mask gated by `pii:unmask` scope. `redact.py:56–111` |
| Media processors | BUILT | convert-to-wav, extract-metadata, probe, slice, export-subtitles (VTT-escaped), export-bundle, ingest-url (SSRF-guarded, 1 GiB cap, sha256). Deterministic ids → idempotent. `processors/*` |
| Job pipeline | BUILT | NATS JetStream, durable consumer, per-org concurrency cap (8), atomic queued→running claim, capped exp. backoff, DLQ + requeue, outbox→HMAC webhook delivery (SSRF-guarded, SKIP LOCKED). `worker.py:173–314`, `delivery.go:52–114` |
| Result cache | BUILT | Content-addressed `sha256(input ‖ params ‖ model_version)`, org-scoped; modes auto/bypass/only; hits cost $0; `/cache/stats` savings. `cache.go:29–131` |
| Cost model | PARTIAL | Flat: `cost = duration × 0.00005` (single CPU-second constant) overwrites per-job rate; streaming `= audio_seconds × 0.0001` (client-supplied). **No GPU metering, no LLM token pass-through.** `config.py:20–22`, `worker.py:248` |
| Tenancy & auth | BUILT | Argon2id API keys (scoped, `*`=full org), Keycloak JWT (org_id→RLS), **FORCE RLS**, Redis sliding-window rate limits (fails open), audit log, GDPR erasure saga, SSRF guards. `apikey.go`, `db.go:61–81`, `erasure/service.go` |
| Budgets | PARTIAL | **Advisory only** — polling loop fires threshold alerts + `usage.budget_threshold` events; job creation never consults budgets; **nothing caps spend.** `usage/service.go:113–171` |
| Marketplace / BYO-model | PARTIAL | Submit→review→promote as `community`. **Metadata only** — no code upload/sandbox/execution. Model registry (S3, sha256-verified) exists but not wired to runtime loading. `marketplace.go:154–219` |
| GPU / batching / autoscale | MISSING | No `device="cuda"`; GPU tier enum exists but no processor declares one. `batching` pkg aggregates job _results_, not GPU inputs. JetStream depth gauge exported but no in-app autoscaling. |

**Honest bottom line.** Production-real: Go API, RLS, JetStream pipeline, webhook delivery, cache, GDPR erasure, ffmpeg/ffprobe ops. Demo-grade/default-stubbed: diarization, summarize/translate, ML-PII. Fundamentally coarse: English-only `tiny.en`, flat CPU-second cost, window-re-transcription streaming. Advisory-not-enforced: budgets. Absent: GPU execution, inference batching, autoscaling.

---

## Part 2 — Competitor profiles (as of 2026-08-11)

Prices are vendor pricing pages; accuracy claims are self-published unless noted.

### Deepgram
- **Price:** batch (Nova-3 mono) $0.0077/min · streaming $0.0048/min. Add-ons per-min (redaction/keyterm/diarize).
- **Latency:** <300 ms streaming; Flux voice-agent model p95 ~1.5s turn-detect.
- **Languages:** 50+ (Flux realtime: 10). **RT diarization + word timestamps.**
- **On-prem:** yes (GPU, enterprise). **Compliance:** SOC2, HIPAA, PCI.
- **Standout:** Flux + Keyterm Prompting (claims 90% higher keyword recall). Summarize/sentiment not on current Nova-3 pages (NOT VERIFIED).
- Sources: deepgram.com/pricing, developers.deepgram.com/docs/models-languages-overview

### AssemblyAI
- **Price:** async Universal-2 $0.15/hr, U3.5-Pro $0.21/hr · streaming $0.15–0.45/hr (billed on WS session wall-clock incl. idle). Intelligence as +$/hr add-ons.
- **Latency:** ~300 ms P50, immutable finals; unlimited concurrency.
- **Languages:** 99+ (U2). **RT diarization** (+$0.12/hr).
- **On-prem:** enterprise + EU residency. **Compliance:** SOC2, ISO, HIPAA (BAA), 99.9% SLA.
- **Standout:** broadest audio intelligence (PII, entities, sentiment, topics, chapters, moderation, translation) + **LeMUR** LLM-over-transcript + LLM gateway.
- Sources: assemblyai.com/pricing, assemblyai.com/universal-streaming, assemblyai.com/security

### OpenAI (hosted STT)
- **Price:** batch $0.003–0.006/min (`gpt-4o-mini-transcribe` $0.003, `gpt-transcribe` $0.0045, `gpt-4o-transcribe` $0.006, whisper-1 $0.006 legacy) · realtime `gpt-live-transcribe` $0.017/min.
- **Latency:** tunable delay tiers; **ms unpublished (NOT VERIFIED).**
- **Languages:** ~98 (whisper-1). **RT diarization: NO** (batch-only `gpt-4o-transcribe-diarize`).
- **On-prem:** hosted API no; **open Whisper weights (MIT) self-hostable** (not identical to hosted models). **Compliance:** HIPAA via BAA.
- **Standout:** open-weights Whisper + GPT ecosystem. **Publishes no WER, no latency.**
- Sources: developers.openai.com/api/docs/pricing, .../guides/realtime-transcription

### Gladia
- **Price:** batch $0.61/hr (Growth $0.20) · realtime $0.75/hr (Growth $0.25). Diarize/sentiment/NER/summary/PII/translation **bundled**.
- **Latency:** <103 ms partials (Solaria-1); ~270 ms overall.
- **Languages:** 100+, native code-switching. **RT diarization: yes.**
- **On-prem:** enterprise dedicated infra (no true self-host, NOT VERIFIED). **Compliance:** GDPR, HIPAA, SOC2, ISO.
- **Standout:** bundled audio intelligence + published full WER table; EU-strong. Concurrency Starter 30 RT / 25 async.
- Sources: gladia.io/pricing, gladia.io/solaria-3

### Speechmatics
- **Price:** from $0.129/hr (batch vs realtime split NOT cleanly published; 3rd-party ~$0.30 batch / ~$0.40 realtime, NOT VERIFIED). 20% off >500 hrs/mo.
- **Latency:** <1s finals, few-hundred-ms partials.
- **Languages:** 56+ · 69 translation pairs. **RT diarization: yes (50–100 speakers)** + enrollment speaker ID.
- **On-prem:** **container / air-gapped** (differentiator). **Compliance:** ISO, SOC2, HIPAA, GDPR.
- **Standout:** on-prem + strongest non-English/European accuracy positioning.
- Sources: speechmatics.com/pricing, docs.speechmatics.com

### Rev AI
- **Price:** Reverb $0.20/hr, Turbo $0.10/hr, Foreign $0.30/hr; Whisper $0.005/min; Human $1.99/min. Insights (summary/sentiment/topic/translate) as add-ons.
- **Latency:** partial+final; **ms unpublished (NOT VERIFIED).**
- **Languages:** 58+ async · **9 streaming.** **RT diarization: yes** (speaker-switch). Custom vocab 6,000 phrases.
- **On-prem:** **Docker + open-source Reverb model** (~600M, non-commercial). **Compliance:** SOC2, HIPAA, PCI, CJIS.
- **Standout:** open-source Reverb + human transcription. **PII redaction NOT VERIFIED**; streaming second-class.
- Sources: rev.ai/pricing, docs.rev.ai, github.com/revdotcom

### ElevenLabs Scribe
- **Price:** batch (Scribe v2) $0.22/hr (+entity $0.07, +keyterm $0.05) · realtime $0.39/hr list.
- **Latency:** ~150 ms (marketing 30–80 ms).
- **Languages:** 90+ (v2). **RT diarization: NO** (batch only, 32 spk).
- **On-prem:** **no** (enterprise isolated only). **Compliance:** SOC2, ISO, PCI, HIPAA (BAA), US/EU/India residency.
- **Standout:** accuracy claims (v1 EN ~96.7%), audio-event tagging (batch). No summarization/translation; no RT diarization.
- Sources: elevenlabs.io/pricing/api, elevenlabs.io/docs

### Google Cloud STT v2 / Chirp
- **Price:** $0.016/min standard (batch & streaming same); **24h dynamic batch ≈$0.004/min (75% off)**; volume tiers to $0.004; medical (v1) $0.078/min; on-prem $0.024/min.
- **Languages:** 125 service-wide (Chirp 3 ~99). **Diarization: Chirp 3 only (14 langs).** Chirp 3 **dropped word timestamps & translation.**
- **On-prem:** Anthos / GDC container. **Compliance:** HIPAA (BAA), 99.9% SLA. PII via Cloud DLP; summary via Gemini.
- Sources: cloud.google.com/speech-to-text/pricing (renders dynamically — some figures via secondary trackers, NOT FULLY VERIFIED)

### AWS Transcribe
- **Price:** batch $0.006/min (T1→$0.0025 T3) · streaming $0.010/min. PII +$0.0024/min. Call Analytics post-call $0.030/min. Medical batch $0.075/min.
- **Languages:** 100+ batch · ~54 streaming. **Diarization: yes.** **Native PII redaction in batch AND streaming.** Call Analytics (sentiment/categories/summarization, post-call).
- **On-prem:** **none.** **Compliance:** HIPAA-eligible; no published uptime SLA. Concurrency: 250 batch jobs / 25 streams.
- **Standout:** native PII + Call Analytics; cleanest hyperscaler pricing.
- Sources: aws.amazon.com/transcribe/pricing, .../faqs

### Azure AI Speech
- **Price (secondary — page renders dynamically, NOT FULLY VERIFIED):** realtime ~$1.00/hr, batch ~$0.18/hr, fast transcription ~$0.36/hr, custom realtime ~$1.20/hr. F0 free 5 hrs/mo.
- **Languages:** 100+ locales. **Diarization: yes (up to 35 spk).** First-class speech translation. **Speaker Recognition/verification RETIRED Sep 2025.**
- **On-prem:** **Docker connected + disconnected (offline license).** **Compliance:** HIPAA (BAA), 99.9% SLA. PII via Azure AI Language; summary via Azure OpenAI. Realtime concurrency 100 (shared STT+translation).
- Sources: learn.microsoft.com/azure/ai-services/speech-service, azure.microsoft.com/pricing/details/speech

---

## Part 3 — Feature-parity matrix

`●` shipped · `◑` partial / add-on / stub-default · `✕` absent · `n-v` not verified. Orpheus column first.

| Capability | **Orpheus** | Deepgram | AssemblyAI | OpenAI | Gladia | Speechmatics | ElevenLabs | AWS |
|---|:--:|:--:|:--:|:--:|:--:|:--:|:--:|:--:|
| **Core ASR** | | | | | | | | |
| Batch transcription | ● | ● | ● | ● | ● | ● | ● | ● |
| Multilingual (>20 langs) | ✕ | ● | ● | ● | ● | ● | ● | ● |
| Word-level timestamps | ● | ● | ● | ◑ | ● | ● | ● | ● |
| Modern model tier | ✕ | ● | ● | ● | ● | ● | ● | ● |
| **Realtime / streaming** | | | | | | | | |
| True streaming ASR | ◑ | ● | ● | ● | ● | ● | ● | ● |
| Sub-300 ms partials | ✕ | ● | ● | n-v | ● | ● | ● | ◑ |
| Realtime diarization | ✕ | ● | ● | ✕ | ● | ● | ✕ | ● |
| VAD endpointing | ✕ | ● | ● | ● | ● | ● | ● | ● |
| **Audio intelligence** | | | | | | | | |
| Speaker diarization | ◑ | ● | ● | ◑ | ● | ● | ◑ | ● |
| PII redaction | ◑ | ● | ● | ✕ | ● | ● | ◑ | ● |
| Summarization | ◑ | n-v | ● | ◑ | ● | ● | ✕ | ◑ |
| Translation | ◑ | n-v | ● | ◑ | ● | ● | ✕ | ✕ |
| Sentiment / topics | ✕ | n-v | ● | ✕ | ● | ● | ✕ | ● |
| Custom vocab / keyterms | ✕ | ● | ● | ● | ● | ● | ● | ● |
| LLM-over-transcript | ✕ | ✕ | ● | ● | ◑ | ✕ | ✕ | ◑ |
| **Cost & deployment** | | | | | | | | |
| GPU inference | ✕ | ● | ● | ● | ● | ● | ● | ● |
| Self-host / on-prem | ● | ● | ◑ | ◑ | ✕ | ● | ✕ | ✕ |
| Multi-tenant RLS SaaS | ● | ✕ | ✕ | ✕ | ✕ | ✕ | ✕ | ✕ |
| Hard budget enforcement | ◑ | n-v | n-v | ● | n-v | n-v | n-v | ● |
| **Developer & enterprise** | | | | | | | | |
| Webhooks / async | ● | ● | ● | ◑ | ● | ● | ● | ● |
| Multi-language SDKs | ✕ | ● | ● | ● | ● | ● | ● | ● |
| HIPAA / SOC2 | ◑ | ● | ● | ● | ● | ● | ● | ● |
| GDPR data erasure | ● | n-v | n-v | n-v | ● | n-v | ● | n-v |
| Processor marketplace | ◑ | ✕ | ✕ | ✕ | ✕ | ✕ | ✕ | ✕ |

**Read:** Orpheus's `✕` cluster is concentrated in ASR quality, streaming, and audio-intelligence; its `●` cluster (RLS SaaS, self-host, GDPR, marketplace) is exactly where the pure-API vendors are blank. That inversion is the strategic story.

---

## Part 4 — Performance · latency · cost deep-dive (priority focus)

RTFx below are batch-ideal on A100-80GB; real effective throughput runs **2–5× lower**. Core formula: `$/audio-hr = (GPU $/hr) ÷ RTFx`.

### The streaming anti-pattern (fix first)
Orpheus re-runs full Whisper over the entire growing 3s window every partial → cumulative compute **O(n²)** in window count; latency scales with window length. Cache-aware streaming models encode each frame **exactly once**.
- **Short term:** LocalAgreement-n over Whisper — confirm a prefix when _n_ chunk updates agree; ~3.3s latency, bounded recompute (arxiv 2307.14743, github ufal/whisper_streaming).
- **Proper:** cache-aware streaming model (Parakeet / Nemotron streaming) — sub-second interims, configurable 80–1120 ms chunks (hf: nvidia/nemotron-3.5-asr-streaming-0.6b).
- **VAD endpointing** (Silero) skips silence, drives interim/final cadence.

### Model frontier (Open ASR Leaderboard, English)
| Model | Params | RTFx ↑ | WER % ↓ | Read |
|---|--:|--:|--:|---|
| `tiny.en` **(Orpheus default)** | 39M | very high | ~high | Fast but weak, English-only — the current floor |
| whisper large-v3 | 1.54B | 146 | 7.44 | Multilingual reference; slow |
| whisper large-v3-turbo | 809M | 200 | 7.83 | **Drop-in Whisper upgrade** — ~2× long-form, +0.4pp WER, keeps multilingual |
| distil-large-v3.5 | ~756M | 202 | 7.21 | English; slightly faster _and_ more accurate than turbo |
| canary-1b | 1B | 235 | 6.5 | NeMo; canary-qwen-2.5b **leads the board at 5.63** |
| parakeet-tdt-0.6b-v2 | 0.6B | **3390** | **6.05** | **Frontier:** ~17× faster than turbo _and_ more accurate. English (v3 multilingual 6.32). NVIDIA-only |

Sources: arxiv.org/html/2510.06961v4, hf model cards.

### Free wins on the current stack
- **CTranslate2 int8** — ~35% less VRAM, ~same/faster than fp16, <1% WER hit (github SYSTRAN/faster-whisper bench). Make it the default compute type.
- **WhisperX** — batched faster-whisper (~70× realtime, <8 GB VRAM) + wav2vec2 forced alignment for **±50 ms word timestamps** + built-in VAD (github m-bain/whisperX). Highest-leverage single library swap for batch.
- **Un-hardcode `language="en"`** — Whisper already returns detected language; one line → 90+ languages.

### Cost frontier — $/audio-hour
Self-host = A100-80GB @ $2.50/hr ÷ leaderboard RTFx (best case). The number to beat is **Groq $0.04**, not OpenAI $0.36.

| Path | $/audio-hr | Notes |
|---|--:|---|
| Self-host · parakeet-tdt | $0.0007 | Batch-ideal; ~490× under OpenAI. Realistic single-GPU far higher |
| Self-host · large-v3-turbo | $0.0125 | ~$0.023 realistic on L4 (~35× RTFx) |
| **Groq · whisper turbo** | **$0.04** | Real floor for a hosted API — beats naive self-host once idle/ops/cold-start counted |
| OpenAI · gpt-4o-mini-transcribe | $0.18 | whisper-1 legacy $0.36 |
| AssemblyAI · Universal-2 | $0.21 | + per-hour intelligence add-ons |
| Deepgram · Nova-3 batch | $0.46 | Premium; sub-300 ms streaming |
| **pyannote diarization** | ~$0.02 | **Slower than the ASR itself** — roughly doubles pipeline cost; true throughput floor; partly CPU-bound |

Sources: modal.com/pricing, runpod.io/pricing, console.groq.com, hf pyannote card.

### Serving economics
- **Batching is the cost lever** (10–70× RTFx). Only the async/offline path can fill batches — the one regime where self-host beats APIs.
- **Split the architecture:** batched GPU pool for bulk, small warm-pool streaming path for live (single-stream RTFx collapses → economics flip to hosted).
- **Serverless GPU** (Modal L4 ~$0.80/hr, A100-80 $2.50; RunPod L40S $0.99) with scale-to-zero suits bursty load; cold starts hurt live → keep a minimal warm pool.

**Cost reality check.** Self-host batched is 20–490× cheaper than OpenAI _on paper_, but **diarization — not transcription — dominates full-pipeline cost**, and Groq's $0.04/hr already undercuts naive self-host once idle/cold-start overhead is counted. Self-hosting wins decisively only at sustained high volume, or for data residency / custom models / tightly-coupled diarization+alignment. Orpheus's flat `$0.00005/CPU-second` models none of this — it can't prove savings or protect a customer with a cap.

---

## Part 5 — Market gaps & differentiation

**What rivals ship that we lack:** sub-300 ms true streaming + realtime diarization/endpointing; keyterm/custom-vocab boosting (table stakes); bundled real audio intelligence (sentiment/topics/chapters/entities) + LLM-over-transcript; native in-stream PII; modern models with published accuracy and multiple speed/accuracy tiers.

**What could differentiate Orpheus:** multi-tenant **RLS SaaS** (no pure-API player exposes true per-tenant DB isolation); credible **self-host + on-prem** paired with SaaS economics (only Speechmatics/Rev have on-prem, neither with a marketplace); **transparent per-second, GPU-metered pricing** (wedge vs opaque per-hour+add-on billing — _if_ the cost model becomes real); an **extensible processor marketplace** (platform play no ASR vendor offers — once it runs sandboxed code, not metadata); **composable pipelines** (transcribe→diarize→redact→summarize→export) already modeled as jobs.

---

## Part 6 — Prioritized roadmap

Tags: `[parity]` `[perf]` `[diff]` + impact (H/M/L) + effort. Do ASR-quality & latency parity first; the wedge only matters once the core keeps up.

### Wave 1 — Now (close the ASR-quality & latency gap; mostly config/library swaps)
- **Kill the 3s streaming window** — LocalAgreement-n over Whisper (bounded recompute, ~3.3s). `[perf]` H · med
- **Upgrade batch model + GPU + int8** — default large-v3-turbo (multilingual) or Parakeet-TDT (EN); `device=cuda`, `compute_type=int8`. `[perf]` H · med
- **Unlock multilingual** — stop forcing `language="en"`; use Whisper detection. `[parity]` H · low
- **Accurate word timestamps via WhisperX** — batched inference + wav2vec2 alignment + VAD. `[parity]` M · low-med
- **Dynamic GPU batching (async path)** — batch VAD chunks through a warm pool; 10–70× lever. `[perf]` H · med

### Wave 2 — Next (make audio-intelligence real; cost/streaming production-grade)
- **Real diarization, on-demand** — pyannote default (not the stub); opt-in per job (cost floor); add realtime diarization. `[parity]` H · med-high
- **Productionize PII / summarize / translate** — Presidio default; managed LLM w/ token-cost pass-through; stop advertising stubs. `[parity]` M · med
- **Cache-aware streaming model** — Parakeet/Nemotron streaming for single-pass incremental decoding. `[perf]` H · high
- **Real GPU-metered cost + hard budgets** — meter GPU-seconds & LLM tokens; budgets _block_ at cap. `[diff]` M · med
- **Keyterm / custom-vocab boosting** — initial-prompt biasing + streaming hotwords. `[parity]` M · med
- **Client SDKs + async callbacks polish** — Python/JS/Go. `[parity]` M · med

### Wave 3 — Later (lean into the wedge)
- **Self-host / on-prem packaging** — shippable air-gapped deployment + transparent pricing pitch. `[diff]` H · med
- **Sandboxed processor marketplace** — real third-party execution (registry already exists). `[diff]` H · high
- **LLM-over-transcript (LeMUR-style)** — Q&A/chapters/custom summaries over stored transcripts. `[parity]` M · med
- **Sentiment / topics / audio events** — reach AssemblyAI/Gladia breadth. `[parity]` L-M · med
- **Autoscaling workers on queue depth** — wire the JetStream-depth gauge to an HPA. `[perf]` M · med

---

## Verification caveats
- Competitor pricing/latency change frequently; drawn from official pages where machine-readable. **Azure & Google pricing tables render dynamically** and lean partly on secondary trackers (flagged inline).
- Leaderboard RTFx are batch-ideal on A100-80GB — real effective throughput 2–5× lower.
- Independent third-party WER is largely unavailable; vendor accuracy claims are self-published.
- Items the research could not confirm are marked **NOT VERIFIED**.
- Orpheus claims cite `file:line` in the `main` branch as of 2026-08-11.
