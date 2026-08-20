# PRD: Frontier / Niche Realtime Capabilities

**Status:** Proposed · **Priority:** P3 · **Epic:** Frontier / Niche · **Related issues:** #377, #378, #379, #380, #383, #384

## 1. Summary

This PRD scopes six frontier capabilities that extend Orpheus's realtime
streaming pipeline beyond transcription: realtime speaker enrollment / voiceprint ID (#377),
realtime audio-event detection (#378), realtime PII redaction of the *audio itself* (#379),
realtime emotion / acoustic-scene / age-gender inference (#380), ambient background-sound
injection for AI agents (#383), and AI QA auto-scoring of calls (#384). Each builds on
infrastructure Orpheus already has — the streaming WebSocket relay, the diarization service's
speaker embeddings, and the LLM service — rather than net-new platforms. The goal is to
separate genuine market white-space (things no vendor ships in realtime) from app-layer UX that
belongs outside the platform, and to specify each in-scope capability as a **complete,
production-grade** feature — multi-tenant, consent/RLS-governed, latency-budgeted, fail-safe,
metered, and observable.

Delivery is ordered into **Milestones (M1–M4)**; each milestone is itself production-quality and
shippable (not a spike or a throwaway prototype). Two items get an explicit **in-platform vs
out-of-scope** classification: #383 (ambient injection) lands as a thin licensed primitive rather
than a platform build, and #380's age-gender sub-signal is policy-gated and off by default.

## 2. Motivation & goals

- **Differentiation:** Orpheus's realtime relay + non-gated speaker embeddings + open LLM are a
  strong base for capabilities that batch-only competitors and closed vendors don't offer live.
- **Classify honestly:** some of these are true white-space (realtime audio PII redaction,
  realtime voiceprint ID at the platform layer); others are largely application UX (Talon-style
  agent assist) that we should *enable* via primitives, not *build* into the platform.
- **Ship production, not demos:** each in-scope capability is specified as a full feature with a
  defined latency budget, failure/degradation behavior, multi-tenant isolation, biometric/consent
  governance where relevant, cost metering, and observability — not a hypothesis to be revisited.
- **Non-goals:** training custom models where an open pretrained one exists; building end-user
  agent applications (that's the customer's layer); age-gender inference by default (policy-gated,
  off).

## 3. Current state in Orpheus

- **Streaming relay:** `handlers/streaming_ws.go` — `StreamTranscribe` (`:114`) relays PCM16 mono
  16 kHz frames (`streamSampleRate=16000`, `:44`) browser↔worker, bidirectionally
  (`relay`, `:169`), with server-side byte metering (`:196`). Every frontier feature that consumes
  or transforms the live audio hooks here or in the worker's `/v1/stream/transcribe`.
- **Speaker embeddings (the voiceprint substrate):** `infra/modal/orpheus_diarize.py` runs
  SpeechBrain **ECAPA-TDNN** speaker embeddings (Apache-2.0, non-gated, `:3`) over VAD-gated
  windows, clustered into turns; returns `{turns:[{start,end,speaker}], num_speakers, gpu_seconds}`
  (`:8-9`, `:107`). ECAPA embeddings are exactly what voiceprint enrollment/ID needs — the model
  is already deployed.
- **LLM service (the scoring/redaction-reasoning substrate):** `infra/modal/orpheus_llm.py` —
  Qwen2.5-3B-Instruct on A10 (`:6`), used for summarize/translate; the natural engine for QA
  auto-scoring rubrics.
- **Text PII redaction (prior art to extend to audio):** `docs/prd/08-pii-redaction.md` already
  specs entity types, masking modes, and a `pii:unmask` scope (`auth/principal.go` scope table);
  #379 extends that from text to the audio waveform.
- **GPU metering:** all Modal services return `gpu_seconds` (`orpheus_transcribe.py:142`,
  `orpheus_diarize.py:107`) so any new GPU feature slots into existing cost accounting
  (`usage/service.go` rollup).
- **Governance primitives:** RLS-scoped org data, the erasure saga (`handlers/erasure.go`), and the
  ZDR/privacy toggle (PRD 11 / prd-14 §4.8) are the levers biometric consent and retention hook
  into.

## 4. Proposed design

Each capability is scoped as: **classification → production feature → where it runs → governance,
failure modes, scale, observability.** All live side-channel events are emitted on the existing WS
as **additive** message types (`{type, ...}`), so a client that ignores unknown types is
unaffected — the on-wire contract stays backward-compatible.

**4.1 Realtime speaker enrollment / voiceprint ID (#377) — genuine white-space (platform).**
No mainstream realtime API ships live voiceprint identification. *Feature:* an enrollment
endpoint stores an org-scoped ECAPA embedding (reuse `orpheus_diarize.py`'s embedding
extraction, `:61`) as a labeled voiceprint; during a live session, the worker computes a rolling
embedding on the relay's audio and cosine-matches against enrolled prints, emitting `speaker_id`
events on the WS alongside partial/final transcripts, each with a match score and an
`unknown` fallback below threshold. *Where:* embedding extraction on the diarize Modal GPU;
matching in the worker.
*Governance:* voiceprints are biometric data — enrollment requires a recorded consent grant, prints
are **FORCE-RLS org-scoped**, retention-bounded, honored by the erasure saga and ZDR toggle
(PRD 11); ZDR sessions match against nothing persisted and store no rolling embedding.
*Failure/scale:* matcher runs within a bounded per-session compute budget; embedding-service errors
degrade gracefully to transcript-only (no `speaker_id`), never dropping the ASR stream; matching is
capped at the org's enrolled-print count with a ceiling to bound cost.
*Observability/cost:* `gpu_seconds` for embedding extraction metered via `usage/service.go`;
metrics on match rate / unknown rate / latency.

**4.2 Realtime audio-event detection (#378) — white-space (platform).**
Detect non-speech events live (laughter, dog bark, alarm, silence, DTMF, hold music). *Feature:*
a pretrained audio tagger (YAMNet/PANNs-class, open) runs on the relay's PCM frames in parallel
with ASR, emitting `event` messages `{label, start, end, score}` on the same WS. *Where:* a new
Modal function or in-worker ONNX model, fed the same frames the relay already pumps.
*Failure/scale:* event inference runs on a separate task from ASR so a tagger stall or error never
blocks or delays transcription (graceful degradation to ASR-only); bounded frame buffer applies
backpressure rather than growing unbounded. *Observability/cost:* per-label emission counts +
inference latency metrics; `gpu_seconds` (if Modal) metered per session.

**4.3 Realtime audio PII redaction (#379) — genuine white-space (platform), extends PRD 08.**
Bleep/mute the PII in the *audio* live, not just mask the transcript. *Feature:* streaming ASR
word timestamps (the worker already emits partial/final with timing) feed the PRD-08 entity
detector; on a detected PII entity, the corresponding audio span in the outbound stream is
muted/toned before it reaches the recording/downstream consumer. *Where:* in the relay
(`streaming_ws.go` worker→browser leg, `:185`) with a short jitter buffer so redaction can act
before emit. This is the hard, valuable one — realtime audio redaction, PCI/HIPAA-grade, is not
offered live by vendors.
*Fail-safe (the core guarantee):* on detector uncertainty, timing ambiguity, or detector error the
span is **muted, not leaked** — redaction fails closed. The jitter buffer is bounded; if the
latency budget would be exceeded the system still errs toward muting. Both the redacted audio and
the masked transcript are produced so text and audio redaction agree.
*Governance/scale:* redaction policy is per-tenant; ZDR sessions never persist the pre-redaction
audio; buffer memory is bounded per session. *Observability:* metrics on entities muted, added
latency (p50/p95), and fail-safe activations.

**4.4 Realtime emotion / acoustic-scene / age-gender (#380) — partial white-space (platform,
staged).** Emit paralinguistic signals live: emotion/sentiment, acoustic scene (call-center vs
outdoor), coarse age/gender. *Feature:* **emotion + acoustic-scene** ship as WS side-channel events
(open models, low ethical risk), same additive pattern as 4.2. **Age-gender is gated behind an
explicit, region-aware policy flag, default off**, and only enabled per-tenant after a documented
policy/consent review — its accuracy and bias/consent profile make default-on out of scope.
*Where:* Modal GPU inference on relay frames. *Governance:* sensitive-signal events honor consent
and ZDR; age-gender emission is refused (not silently omitted) when the policy flag is off.
*Failure/scale:* runs on a separate task from ASR (degrades to ASR-only on error); bounded compute;
metered via `gpu_seconds`. *Observability:* per-signal emission + latency metrics; policy-flag state
audited.

**4.5 Ambient / background-sound injection for agents (#383) — enabling primitive (explicitly not a
platform build).** Inject controllable ambient sound (call-center hum, office noise) into an AI
agent's *outbound* audio so it sounds natural. *Classification decision (recorded):* the
audio-mixing itself is trivial and belongs in the customer's agent app, so Orpheus ships **only a
thin primitive** — a curated, licensed ambient-loop library plus a mix parameter on an outbound
TTS/stream endpoint — and does **not** build agent-side mixing UX. Even as a primitive it is
production-grade: loops are license-cleared and checksummed in the model/asset registry, the mix
parameter is validated and bounded, and usage is metered. *Stance:* **enable, don't build.**

**4.6 AI QA auto-scoring of calls (#384) — white-space-ish (platform), builds on LLM.**
Auto-score completed calls against a QA rubric (greeting, compliance disclosure, resolution,
sentiment). *Feature:* a post-call async processor takes the transcript (+ diarization turns +
emotion signals from 4.4) and runs a rubric prompt on the existing LLM service
(`orpheus_llm.py`), returning a structured, schema-validated scorecard artifact. *Where:* a standard
async processor in the job pipeline (not realtime) — reuses summarize infra.
*Failure modes:* malformed LLM output is re-prompted/repaired against the scorecard schema, then
dead-lettered with a clear reason rather than emitting a partial scorecard; rubric prompts are
versioned so scores are reproducible. *Governance/scale:* transcript access is RLS-scoped; the
processor honors ZDR (no scorecard persisted when the source job is ZDR); LLM calls are bounded and
metered via `gpu_seconds`. *Observability:* per-rubric-item score distribution + processing metrics.

**Classification summary:**
- *Genuine realtime white-space (build, production):* #377 voiceprint ID, #378 audio-event
  detection, #379 audio PII redaction.
- *Staged / policy-gated (build emotion+scene; age-gender gated, default off):* #380.
- *Enable-don't-build (thin licensed primitive only):* #383 ambient injection.
- *Post-call, high-value, low-risk (build on LLM):* #384 QA auto-scoring.

## 5. Rollout / milestones

Each milestone is independently production-quality and shippable — governed, fail-safe, metered,
observable. Milestones order the work; none is a spike or reduced-scope prototype.

1. **M1 — Live side-channel foundation.** #378 audio-event detection and #377 voiceprint
   enrollment/ID: establish the additive WS side-channel contract, the separate-task
   (ASR-non-blocking) inference pattern, RLS/consent governance for voiceprints, and metering.
2. **M2 — QA auto-scoring (#384).** Production async processor: schema-validated scorecards,
   versioned rubrics, malformed-output repair, ZDR/RLS honoring, `gpu_seconds` metering.
3. **M3 — Realtime audio PII redaction (#379).** The flagship differentiator: bounded jitter
   buffer, word-timing integration, fail-closed muting, text/audio agreement, latency budget.
4. **M4 — Paralinguistic + primitive.** #380 emotion/scene as governed side-channel events
   (age-gender policy-gated, default off); #383 shipped as the thin licensed ambient primitive.

## 6. Verification / acceptance criteria

End-to-end against a **real worker + streaming server + Modal** (not unit-only), with negative/
failure paths, numeric targets, and multi-tenant isolation gates.

- **#377 voiceprint ID.** An enrolled voiceprint is matched live during a streaming session with
  documented precision/recall on a held-out set (state target, e.g. precision ≥ 0.9 @ recall ≥ 0.8);
  `speaker_id` events arrive within the streaming latency budget (p95 added latency ≤ target);
  below-threshold audio yields `unknown`, not a false match. Isolation: org A's enrolled prints are
  never matched against or visible in org B's session (RLS test). Erasure: an erased voiceprint is
  no longer matchable; a ZDR session persists no rolling embedding. Degradation: with the embedding
  service forced to error, the ASR stream continues uninterrupted (no `speaker_id`).
- **#378 audio-event detection.** Labels are emitted live with measured accuracy on a labeled event
  set (state target); running the tagger in parallel adds **no** measurable regression to ASR
  partial/final latency (within noise); a forced tagger error degrades to ASR-only with the stream
  intact. `gpu_seconds` appears in the usage rollup.
- **#379 audio PII redaction.** PII audio spans are muted before emit at ≥ target recall on a
  labeled PII call set; measured added latency (p50/p95) is within the stated budget; injected
  detector uncertainty/errors result in **muting, not leakage** (fail-closed assertion); the masked
  transcript and the redacted audio agree on every masked span. ZDR: no pre-redaction audio is
  persisted.
- **#380 emotion/scene.** Emotion/scene events are emitted live with published accuracy on a
  labeled set; parallel inference adds no ASR-latency regression; **age-gender is refused when the
  policy flag is off** (explicit refusal, audited) and only emitted when enabled per-tenant.
- **#383 ambient injection.** The classification decision (primitive-only, not platform build) is
  recorded; the primitive's mix parameter produces correct blended outbound audio using
  license-cleared, checksummed loops, with the parameter validated/bounded and usage metered.
- **#384 QA auto-scoring.** A schema-valid scorecard artifact is produced for a completed call on
  the live LLM service; rubric outputs are structured and reproducible across runs of the same
  versioned rubric; malformed LLM output is repaired-or-dead-lettered, never emitted partial; the
  processor honors ZDR (no scorecard when source is ZDR) and RLS (org B cannot read org A's
  scorecard); cost is accounted via `gpu_seconds`.
- **Cross-cutting isolation & metering.** A scripted two-org run confirms no side-channel event,
  voiceprint, redaction policy, or scorecard crosses org boundaries, and produces a per-org usage
  rollup reconciling with emitted `gpu_seconds` metrics.

## 7. Dependencies, risks, open questions

- **Dependencies:** streaming relay word-level timing exposed to the API (for #379); open
  pretrained models for audio tagging/emotion (#378/#380); the PRD-08 entity detector (#379); the
  LLM service (#384); the diarize embedding path exposed as a reusable extractor (#377); the ZDR/
  erasure governance from PRD 11 (all sensitive-data items).
- **Risks:** *Latency* — realtime audio redaction (#379) and any inline transform add delay to a
  path optimized for low latency; the bounded jitter buffer trades latency for correctness and
  fails closed. *Ethics/compliance* — voiceprints (#377) and age-gender (#380) are biometric/
  sensitive; require recorded consent, retention limits, bias review, and must honor the ZDR/
  erasure story from PRD 11. *Scope creep* — #383 is deliberately constrained to a thin primitive to
  avoid building customer app-UX into the platform.
- **Open questions:** run parallel inference (#378/#380) on the same Modal GPU as ASR or a separate
  function (cost vs latency trade)? What is the ratified acceptable added latency for #379? Under
  which regions/policies, if ever, is age-gender enabled? How long are voiceprints retained per org
  by default, and do they cross sessions only within an org?

## 8. Effort

Production builds (not spikes):
- #378 audio-event detection: **S–M** (open tagger on existing frames + side-channel contract).
- #384 QA auto-scoring: **M** (async processor on existing LLM + schema/repair + governance).
- #377 voiceprint enrollment/ID: **M** (reuse ECAPA; add RLS store, live matcher, consent/erasure).
- #380 emotion/scene: **M**; age-gender: policy-gated, effort after review.
- #379 realtime audio PII redaction: **L** (bounded jitter buffer + timing + fail-closed; the hard one).
- #383 ambient injection: **XS–S** as the licensed primitive.
- **Total:** ~2.5–3.5 months across M1–M4, each milestone shippable on completion.
