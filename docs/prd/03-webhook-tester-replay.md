# PRD 03 — Webhook endpoint tester + delivery replay UI (self-serve debugging)

**Status:** Draft · **Owner:** Notifications · **Reviewers:** Platform, DX, Security
**Related:** PRODUCTION_DESIGN §8.1, §8.6 (HMAC signature), existing
`/v1/webhooks/{id}/deliveries` and `.../deliveries/{delivery_id}/replay`

## 1. Problem & motivation

Webhook delivery, signing (HMAC-SHA256), retry/backoff, and single-delivery replay already
exist. What's missing is the **self-serve debugging loop**: developers integrating webhooks
can't (a) test-fire a sample event before wiring real jobs, (b) see *why* a delivery failed
(status code, response body, signature they should have computed), or (c) bulk-replay after
fixing their endpoint. Every failed integration becomes a support ticket. This feature turns
the existing delivery data into a debuggable, mostly self-serve surface.

## 2. Goals / non-goals

**Goals**
- **Test-fire**: send a synthetic, correctly-signed event to an endpoint on demand.
- **Inspect**: per-delivery detail — request headers/body, response code/body snippet, timing,
  the signature base string, and next retry time.
- **Replay**: single (exists) + **bulk** replay by filter (event type, status, time range).
- Surface `exhausted` deliveries prominently and offer re-enable of auto-disabled endpoints.

**Non-goals**
- Building the admin web UI itself in this PRD (Next.js dashboard is separate); we ship the
  **API** it needs. UX described so the frontend can be built against it.
- Changing the signing scheme or delivery retry policy.

## 3. User stories

- As a developer, I want to click "Send test event" and confirm my endpoint returns 2xx.
- As a developer, I want to see the exact bytes and headers of a failed delivery to debug my
  signature check.
- As an on-call dev, I want to replay all `failed`/`exhausted` `job.completed` deliveries from
  the last hour after I fixed a bug.

## 4. Proposed API / UX

```jsonc
// Test-fire a synthetic event (Idempotency-Key honored)
POST /v1/webhooks/{id}/test
{ "event_type": "job.completed" }        // uses a canned sample payload for that type
// 202 → { "delivery_id": "del_...", "poll_url": "/v1/webhooks/{id}/deliveries/del_..." }

// Delivery detail (extend existing WebhookDelivery with a debug view)
GET /v1/webhooks/{id}/deliveries/{delivery_id}
// adds: request_headers, request_body (or payload_url), signature_base_string,
//        response_status, response_headers (subset), response_body_snippet (truncated),
//        attempts[] (each: at, status_code, duration_ms, error), next_retry_at

// Bulk replay by filter
POST /v1/webhooks/{id}/deliveries/replay
{ "event_type": "job.completed", "status": "exhausted",
  "since": "2026-07-15T00:00:00Z", "limit": 500 }
// 202 → { "requeued": 137 }

// Re-enable an auto-disabled endpoint (after 100 DLQs)
POST /v1/webhooks/{id}/enable
```

**UX (dashboard):** endpoint page shows health (recent success rate), a "Send test event"
button, a filterable delivery table, a delivery drawer with request/response and a copyable
signature base string, and "Replay selected"/"Replay all matching".

## 5. Data-model changes

Mostly additive to existing `webhook_deliveries` (org-scoped, RLS, monthly-partitioned):

- Add columns: `response_status int`, `response_body_snippet text` (≤ 4 KB, redacted),
  `signature_base_string text`, `next_retry_at timestamptz`, `is_test bool default false`.
- New `webhook_delivery_attempts(delivery_id, attempt_no, attempted_at, status_code,
  duration_ms, error)` for the per-attempt timeline (RLS, partitioned).
- Reuse existing `payload_url` for full body (S3, signed) to keep large payloads out of PG.

## 6. Edge cases & security

- **Tenant isolation:** all reads/replays are RLS-scoped by `org_id`; delivery IDs are UUID v7
  (non-enumerable) and validated against the endpoint's org.
- **SSRF / abuse:** test-fire and replay reuse the **existing SSRF-safe delivery path** (no private
  IPs, DNS re-resolution guard). Test-fire is rate-limited per endpoint (e.g. 10/min) to prevent
  using Orpheus as a request amplifier.
- **Response body storage:** store only a truncated, size-capped snippet; scrub obvious secrets
  (`authorization`, `set-cookie`) before persisting; snippet honors PRD 08 log redaction rules.
- **Replay safety:** replays reuse the original `event_id` so idempotent consumers dedupe; bulk
  replay is capped (`limit`) and audited (`audit_log`).
- **Auto-disable/re-enable:** re-enable requires `webhook:manage` scope and is audited.

## 7. Metrics / SLAs

- `webhook_delivery_success_ratio` per endpoint, `webhook_test_fire_latency_p95 < 3s`.
- Alert when an endpoint crosses the auto-disable threshold (before it disables).

## 8. Rollout plan

1. Add attempt timeline + response capture columns (write path) — no API change.
2. Ship enriched `GET .../deliveries/{id}` detail + `POST .../test`.
3. Ship bulk replay + re-enable.
4. Frontend consumes; dogfood on Orpheus's own webhooks.

## 9. Open questions

- Retention for `response_body_snippet` (PII risk) — 7 or 30 days?
- Should test events be visibly flagged (`is_test`) to consumers via a header
  (`X-Orpheus-Test: true`)? (Proposed: yes.)
- Do we expose a live "endpoint ping" (HEAD) separate from a full signed test event?
