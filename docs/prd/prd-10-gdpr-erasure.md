# PRD 10 — GDPR erasure endpoint (tenant-initiated hard delete with S3 purge)

**Status:** Draft · **Owner:** Platform + Security · **Reviewers:** Legal, SRE, Billing
**Related:** PRODUCTION_DESIGN §7.1 (lifecycle → tenant-initiated delete), §9.2 ("soft delete for
metadata, hard delete for audio"), §11.1 (Repudiation: append-only audit to S3 Object Lock),
§2.5 (no GDPR/erasure story today)

## 1. Problem & motivation

The design doc commits to right-to-erasure — "actual audio files are deleted from S3" with metadata
soft-deleted and an audit trail retained — but there is **no endpoint** to trigger it. Under GDPR
Art. 17 / CCPA, tenants must be able to erase specific data subjects' data (or all their data), and
we must be able to *prove* the audio bytes are gone (not just soft-flagged). This is a hard
requirement for enterprise/regulated sales and a legal obligation. It must be verifiable, auditable,
and coherent with every other feature that references artifacts (bundles, cache, tenant-S3 pushes).

## 2. Goals / non-goals

**Goals**
- Tenant-initiated **erasure request** scoping what to delete: a specific artifact, all data linked
  to a job, or all data matching a subject reference — with async execution and a verifiable
  completion receipt.
- **Hard delete of audio bytes from S3** (all versions + replicas), soft-delete of metadata rows,
  and cascade to derived artifacts (transcripts, subtitles, bundles, cache entries).
- Tamper-evident audit proof retained (Object Lock) even after the data is gone.

**Non-goals**
- Full account/tenant offboarding (a superset workflow; this covers data erasure primitives).
- Erasing data from the tenant's *own* S3 (PRD 06 destination) — we notify; they own their bucket.
- Legal-hold override logic beyond a simple hold flag (Legal owns that policy).

## 3. User stories

- As a tenant DPO, I want to erase all data for a data subject and receive a certificate of deletion.
- As a tenant, I want to delete one artifact and everything derived from it (transcript, subtitles).
- As an auditor, I want proof (timestamp, scope, operator) that erasure completed and can't be forged.

## 4. Proposed API / UX

```jsonc
POST /v1/erasure-requests        (requires elevated scope data:erase + MFA; Idempotency-Key)
{ "scope": "artifact",           // artifact | job | subject
  "artifact_id": "018e...",      // or job_id, or subject_ref
  "reason": "gdpr_art17",
  "confirm": true }
// 202 → { "id": "era_018f...", "status": "scheduled", "poll_url": "/v1/erasure-requests/era_..." }

GET  /v1/erasure-requests/{id}   // status: scheduled|running|completed|failed;
                                 //   deleted_counts {artifacts, results, bundles, cache_entries},
                                 //   s3_objects_purged, certificate_url (signed, on completed)
GET  /v1/erasure-requests        // list (org-scoped)
```

On completion emit signed webhook `data.erased` `{request_id, scope, counts, completed_at}`. The
`certificate_url` is a signed, downloadable receipt (JSON + human-readable) enumerating what was
purged and confirming S3 object deletion.

## 5. Data-model changes

New org-scoped, RLS-covered tables (goose):

```
erasure_requests( id, org_id, scope, target_id, subject_ref, reason,
    status, requested_by, deleted_counts jsonb, certificate_s3_key,
    scheduled_at, completed_at, error, created_at )

-- add to erasable tables:
artifacts:      deleted_at timestamptz, erasure_request_id uuid   -- soft-delete metadata
job_results:    deleted_at (result JSONB scrubbed of transcript/PII payload on erase)
```

- Execution is a privileged worker workflow: enumerate targets under RLS → **decrement ref-counts**
  (cache/bundle references from PRD 01/02) → delete S3 objects **including all versions and CRR
  replicas** → soft-delete metadata rows → write audit event. Runs as a saga so partial failure is
  resumable and never leaves orphaned bytes.
- Audit rows go to the append-only `audit_log` mirrored to **S3 Object Lock (Compliance mode)** so
  the proof survives and is immutable.

## 6. Edge cases & security

- **Tenant isolation:** an org can only erase its own data; every target resolved under RLS; scope
  `subject` matches only rows tagged with that subject in the org.
- **Reference coherence (critical):** audio referenced by a PRD 01 cache entry or PRD 02 bundle must
  not be resurrectable — erasure decrements refs and force-purges even if other references *would*
  keep it; those derived objects are invalidated (cache entry dropped, bundle marked `revoked`).
- **Verifiability:** after S3 delete, re-`HEAD` the keys (and versions) to confirm 404 before marking
  `completed`; the certificate records the confirmed-purged object list (keys hashed, not exposed).
- **Irreversibility & abuse:** requires `data:erase` scope + MFA + `confirm:true`; heavily audited;
  destructive-by-design. A **legal-hold** flag blocks erasure and returns Problem `type=legal-hold`.
- **In-flight jobs:** erasing an artifact with running jobs cancels them first (reuse cancel path)
  to avoid recreating derived data mid-erase.
- **Backups/WAL:** document that PITR backups (§9.6) age out under their own retention; certificate
  states "purged from primary + replicas; backup rotation completes within N days."
- **Billing:** storage-usage stops accruing at deletion; no charge for erasure.

## 7. Metrics / SLAs

- `erasure_completion_time_p95` (target `< 24h`, most within minutes), `erasure_failure_ratio` (alert
  on any), `s3_purge_verification_success = 100%`.
- SLA surfaced to enterprise: erasure completes within a contractual window (e.g. 30 days per GDPR).

## 8. Rollout plan

1. Add `deleted_at`/soft-delete columns + ref-count decrement plumbing (no endpoint).
2. Ship `artifact`-scope erasure with S3 hard-delete + verification + certificate.
3. Extend to `job` and `subject` scopes + cascade into cache/bundles (PRD 01/02).
4. Add legal-hold flag, `data.erased` webhook, and Object-Lock audit mirroring.

## 9. Open questions

- Subject-linking model: how are artifacts/jobs tagged with a `subject_ref` at ingest so
  subject-scoped erasure is precise? (Needs an ingest-time metadata convention.)
- Backup handling: purge from backups on request (expensive) vs. document rotation window?
- Do we hard-delete `audit_log` entries about the erased data, or keep them (legal basis to retain)?
