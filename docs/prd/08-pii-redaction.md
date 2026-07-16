# PRD 08 — PII redaction in transcripts (and in logs)

**Status:** Draft · **Owner:** Processors & Models + Platform · **Reviewers:** Security, Legal, SRE
**Related:** PRODUCTION_DESIGN §11.1 (STRIDE: "PII redaction in logs"), §11.7-class data protection,
§7.2 result shape · complements PRD 04 (summarize) and PRD 05 (subtitles)

## 1. Problem & motivation

Transcripts routinely contain PII — names, phone numbers, emails, SSNs, credit-card numbers,
addresses. The design doc already commits to **PII redaction in logs** as an information-disclosure
control, but there is no product feature to redact PII in *transcript output*, and no guarantee
that transcript text isn't leaking into operational logs. Regulated tenants (healthcare, finance,
call centers under PCI) cannot use raw transcription without this. This PRD covers **both**: a
tenant-facing redaction processor/option, and a platform-level guarantee that PII in transcripts
does not land in logs.

## 2. Goals / non-goals

**Goals**
- `redact=true` option on transcript-producing processors (and a standalone
  `orpheus.text.redact`) that masks configurable PII entity types in text, segments, and words.
- Output both a redacted transcript and (optionally, permission-gated) an encrypted mapping so
  authorized users can un-redact.
- **Platform guarantee:** transcript/result content is never written to application logs; structured
  logs scrub known-PII fields and free-text bodies.

**Non-goals**
- Audio redaction (bleeping the source audio) in v1 — text-only; audio bleeping is a fast-follow.
- Perfect recall — PII detection is best-effort ML/regex; we document residual risk.
- Redacting arbitrary customer-supplied documents (audio-transcript scope for v1).

## 3. User stories

- As a call-center tenant, I want card numbers and SSNs masked in transcripts before they leave
  the platform.
- As a support engineer, I want to debug a failed job without ever seeing the caller's transcript
  in logs.
- As a compliance officer, I want an audit trail of what entity types were redacted per job.

## 4. Proposed API / UX

```jsonc
// Inline on transcribe / translate / summarize
{ "processor": { "name": "orpheus.audio.transcribe", "version": "1.5.0" },
  "params": { "redact": { "enabled": true,
                          "entities": ["PERSON","PHONE","EMAIL","SSN","CREDIT_CARD","ADDRESS"],
                          "mask": "type",        // type → [PHONE]; char → ●●●; hash → <sha>
                          "keep_mapping": false } } }

// Standalone redaction over an existing transcript
{ "processor": { "name": "orpheus.text.redact", "version": "1.0.0" },
  "params": { "source_job_id": "018f...", "entities": [...], "mask": "type" } }
```

Result: `result.text` / `segments[].text` / `words[].word` are redacted in place; a summary of
`redactions[] = {entity_type, count}` is included. If `keep_mapping=true`, the original↔masked
mapping is written as a **separate, KMS-encrypted** artifact requiring a distinct `pii:unmask` scope
to fetch. Reuse existing `GET /v1/artifacts/{id}/signed-url` for downloads.

## 5. Data-model changes

- **No new core tables.** Redacted transcript replaces/augments `job_results.result`; redaction
  summary lives in `result.redactions`.
- Un-redact mapping is an `artifact` with `sensitivity='pii_mapping'` (new column/flag on
  `artifacts`) → forces stricter access checks + KMS + shorter retention.
- **Logging (platform):** add a redaction pass in the shared logging layer (Go API + Python
  workers) that (a) denylists result/transcript fields from structured logs and (b) scrubs known
  patterns in any free-text log message before emit. Governed by config, applied everywhere via the
  existing logger wrapper.

## 6. Edge cases & security

- **Tenant isolation:** redaction runs inside the tenant's job; mapping artifact is org-scoped/RLS +
  `pii:unmask` scope; even org members without the scope can't un-redact.
- **Log leakage (the core risk):** enforce with a CI lint that fails if job `result`/transcript
  structs are passed to loggers; redaction pass is fail-safe (on error, drop the field, never log raw).
- **Detection gaps:** document that recall is imperfect; offer `mask=hash` for reversible-by-authorized
  workflows without exposing plaintext; allow tenant-supplied custom regex entities.
- **PRD interactions:** redaction runs **before** PRD 04 summarization sees text (so external LLMs
  never receive PII) and before PRD 05 subtitle export writes `.srt`/`.vtt`.
- **Erasure:** mapping artifacts are included in PRD 10 hard-delete and have shortened default retention.
- **Abuse:** redaction adds compute; count toward budgets (PRD 07); cap input length.

## 7. Metrics / SLAs

- `redaction_latency_p95` (small overhead over transcription), redaction coverage on a golden PII set
  (precision/recall, offline eval), target recall published per entity type.
- **Zero** transcript-content log lines in a sampled log audit (SLO: 0 findings/quarter).

## 8. Rollout plan

1. Ship the **log redaction pass** + CI lint first (platform guarantee is table-stakes).
2. Ship `orpheus.text.redact` + `redact` option on transcribe (regex + ML entities).
3. Add KMS-encrypted un-redact mapping + `pii:unmask` scope.
4. Wire ordering guarantees with PRD 04/05; publish per-entity recall.

## 9. Open questions

- Detection engine: self-hosted (Presidio-class) vs. external — external contradicts data-min goals.
- Default entity set per plan/region (GDPR vs. HIPAA vs. PCI differ).
- Retention for un-redact mappings (proposed: ≤ 7 days, tenant-overridable down only).
