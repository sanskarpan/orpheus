// Package handlers — webhook tester + delivery replay (PRD 03).
//
// The self-serve debugging loop over existing delivery data: test-fire a
// synthetic signed event, inspect a delivery (per-attempt timeline, the
// signature base string a receiver must reconstruct, response snippet),
// bulk-replay by filter, and re-enable an auto-disabled endpoint. All reads
// and writes are RLS-scoped; the delivery path (worker) is unchanged and SSRF
// protections still apply to every send.
package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/orpheus/api/internal/audit"
	"github.com/orpheus/api/internal/auth"
	"github.com/orpheus/api/internal/dbtx"
)

// testFireRateLimit caps test-fires per endpoint per minute so Orpheus can't
// be used as a request amplifier.
const testFireRateLimit = 10

// TestFireRequest is the body for POST /v1/webhooks/{id}/test.
type TestFireRequest struct {
	EventType string `json:"event_type"`
}

// sampleEventPayload returns a canned, correctly-shaped payload for an event
// type so developers can validate their endpoint before wiring real jobs.
func sampleEventPayload(eventType string) map[string]any {
	base := map[string]any{"event_type": eventType, "test": true}
	switch eventType {
	case "job.completed":
		base["job_id"] = uuid.NewString()
		base["status"] = "completed"
		base["cache"] = "miss"
	case "job.failed":
		base["job_id"] = uuid.NewString()
		base["status"] = "failed"
		base["error"] = "sample error"
	case "bundle.ready":
		base["bundle_id"] = uuid.NewString()
		base["size_bytes"] = 1024
	}
	return base
}

// TestFire handles POST /v1/webhooks/{id}/test — enqueue a synthetic, signed
// delivery to the endpoint (flagged is_test) that the normal delivery worker
// sends over the SSRF-safe path.
func (h *WebhookHandler) TestFire(w http.ResponseWriter, r *http.Request) {
	p, err := auth.PrincipalFromContext(r.Context())
	if err != nil {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "Unauthorized")
		return
	}
	webhookID, ok := uuidParam(r, "id")
	if !ok {
		writeProblem(w, http.StatusNotFound, "not_found", "Webhook not found")
		return
	}
	var req TestFireRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, "validation", "Invalid JSON")
		return
	}
	if req.EventType == "" {
		req.EventType = "job.completed"
	}

	deliveryID := uuid.NewString()
	eventID := uuid.NewString()
	payload, _ := json.Marshal(sampleEventPayload(req.EventType))
	var rateLimited bool
	err = h.DB.WithTenant(r.Context(), p.OrgID, func(ctx context.Context) error {
		// Endpoint must exist for this org (any active state — a test-fire
		// is allowed even against a disabled endpoint to help debugging).
		var exists bool
		if e := dbtx.QueryRow(ctx, h.DB, `SELECT EXISTS(SELECT 1 FROM webhook_endpoints WHERE id = $1)`, webhookID).Scan(&exists); e != nil {
			return e
		}
		if !exists {
			return pgx.ErrNoRows
		}
		var recent int
		if e := dbtx.QueryRow(ctx, h.DB, `
			SELECT COUNT(*) FROM webhook_deliveries
			WHERE endpoint_id = $1 AND is_test AND created_at > now() - interval '1 minute'
		`, webhookID).Scan(&recent); e != nil {
			return e
		}
		if recent >= testFireRateLimit {
			rateLimited = true
			return nil
		}
		_, e := dbtx.Exec(ctx, h.DB, `
			INSERT INTO webhook_deliveries
			  (id, org_id, endpoint_id, event_type, event_id, payload, status, next_retry_at, attempt_count, max_attempts, is_test, created_at)
			VALUES ($1, $2, $3, $4, $5, $6::jsonb, 'pending', now(), 0, 5, true, now())
		`, deliveryID, p.OrgID, webhookID, req.EventType, eventID, payload)
		return e
	})
	if err != nil {
		if err == pgx.ErrNoRows {
			writeProblem(w, http.StatusNotFound, "not_found", "Webhook not found")
			return
		}
		writeProblem(w, http.StatusInternalServerError, "internal", "Failed to enqueue test event")
		return
	}
	if rateLimited {
		writeProblem(w, http.StatusTooManyRequests, "rate_limited", "Too many test events; try again shortly")
		return
	}
	_ = h.Audit.Record(r.Context(), audit.Entry{
		Action: "webhook.deliver", ResourceType: "webhook", ResourceID: webhookID,
		Metadata: map[string]any{"test": true, "event_type": req.EventType, "delivery_id": deliveryID},
	})
	writeJSON(w, http.StatusAccepted, map[string]any{
		"delivery_id": deliveryID,
		"poll_url":    fmt.Sprintf("/v1/webhooks/%s/deliveries/%s", webhookID, deliveryID),
	})
}

// DeliveryAttempt is one row of the per-attempt timeline.
type DeliveryAttempt struct {
	AttemptNo   int       `json:"attempt_no"`
	AttemptedAt time.Time `json:"attempted_at"`
	StatusCode  *int      `json:"status_code,omitempty"`
	DurationMs  *int      `json:"duration_ms,omitempty"`
	Error       string    `json:"error,omitempty"`
}

// WebhookDeliveryDetail is the enriched debug view of a delivery.
type WebhookDeliveryDetail struct {
	ID                  string            `json:"id"`
	WebhookID           string            `json:"webhook_id"`
	EventID             string            `json:"event_id"`
	EventType           string            `json:"event_type"`
	Status              string            `json:"status"`
	IsTest              bool              `json:"is_test"`
	AttemptCount        int               `json:"attempt_count"`
	RequestHeaders      map[string]string `json:"request_headers"`
	RequestBody         json.RawMessage   `json:"request_body,omitempty"`
	SignatureBaseString string            `json:"signature_base_string,omitempty"`
	ResponseStatus      *int              `json:"response_status,omitempty"`
	ResponseBodySnippet string            `json:"response_body_snippet,omitempty"`
	NextRetryAt         *time.Time        `json:"next_retry_at,omitempty"`
	CreatedAt           time.Time         `json:"created_at"`
	Attempts            []DeliveryAttempt `json:"attempts"`
}

// GetDelivery handles GET /v1/webhooks/{id}/deliveries/{delivery_id} — the
// enriched debug view including the per-attempt timeline.
func (h *WebhookHandler) GetDelivery(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFromContext(r.Context())
	webhookID, ok := uuidParam(r, "id")
	if !ok {
		writeProblem(w, http.StatusNotFound, "not_found", "Webhook not found")
		return
	}
	deliveryID, ok := uuidParam(r, "delivery_id")
	if !ok {
		writeProblem(w, http.StatusNotFound, "not_found", "Delivery not found")
		return
	}

	var (
		d          WebhookDeliveryDetail
		payload    []byte
		sigBase    *string
		snippet    *string
		respStatus *int
		nextRetry  *time.Time
	)
	err := h.DB.WithTenant(r.Context(), p.OrgID, func(ctx context.Context) error {
		if e := dbtx.QueryRow(ctx, h.DB, `
			SELECT id::text, endpoint_id::text, event_id::text, event_type, status::text,
			       is_test, attempt_count, payload, signature_base_string,
			       response_status, response_body_snippet, next_retry_at, created_at
			FROM webhook_deliveries
			WHERE id = $1 AND endpoint_id = $2 AND org_id = $3
		`, deliveryID, webhookID, p.OrgID).Scan(
			&d.ID, &d.WebhookID, &d.EventID, &d.EventType, &d.Status,
			&d.IsTest, &d.AttemptCount, &payload, &sigBase,
			&respStatus, &snippet, &nextRetry, &d.CreatedAt,
		); e != nil {
			return e
		}
		rows, e := dbtx.Query(ctx, h.DB, `
			SELECT attempt_no, attempted_at, status_code, duration_ms, COALESCE(error,'')
			FROM webhook_delivery_attempts WHERE delivery_id = $1 ORDER BY attempt_no
		`, deliveryID)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var a DeliveryAttempt
			if e := rows.Scan(&a.AttemptNo, &a.AttemptedAt, &a.StatusCode, &a.DurationMs, &a.Error); e != nil {
				return e
			}
			d.Attempts = append(d.Attempts, a)
		}
		return rows.Err()
	})
	if err != nil {
		if err == pgx.ErrNoRows {
			writeProblem(w, http.StatusNotFound, "not_found", "Delivery not found")
			return
		}
		writeProblem(w, http.StatusInternalServerError, "internal", "Failed to get delivery")
		return
	}

	d.RequestBody = json.RawMessage(payload)
	d.SignatureBaseString = derefStr(sigBase)
	d.ResponseBodySnippet = derefStr(snippet)
	d.ResponseStatus = respStatus
	d.NextRetryAt = nextRetry
	// The headers the delivery worker sends (the signature value differs per
	// attempt; the base string above is what the receiver reconstructs).
	d.RequestHeaders = map[string]string{
		"Content-Type":      "application/json",
		"User-Agent":        "Orpheus-Webhooks/1.0",
		"Orpheus-Signature": "t=<unix_ts>,v1=<hex_hmac_sha256(base_string)>",
	}
	if d.Attempts == nil {
		d.Attempts = []DeliveryAttempt{}
	}
	writeJSON(w, http.StatusOK, d)
}

// BulkReplayRequest filters which deliveries to requeue.
type BulkReplayRequest struct {
	EventType string     `json:"event_type,omitempty"`
	Status    string     `json:"status,omitempty"`
	Since     *time.Time `json:"since,omitempty"`
	Limit     int        `json:"limit,omitempty"`
}

// BulkReplay handles POST /v1/webhooks/{id}/deliveries/replay — requeue every
// delivery matching the filter as a fresh pending delivery (preserving the
// original event_id so idempotent consumers dedupe).
func (h *WebhookHandler) BulkReplay(w http.ResponseWriter, r *http.Request) {
	p, err := auth.PrincipalFromContext(r.Context())
	if err != nil {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "Unauthorized")
		return
	}
	webhookID, ok := uuidParam(r, "id")
	if !ok {
		writeProblem(w, http.StatusNotFound, "not_found", "Webhook not found")
		return
	}
	var req BulkReplayRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, "validation", "Invalid JSON")
		return
	}
	if req.Status != "" && !validDeliveryStatus(req.Status) {
		writeProblem(w, http.StatusBadRequest, "validation", "invalid status filter")
		return
	}
	limit := req.Limit
	if limit <= 0 || limit > 500 {
		limit = 500
	}
	var since time.Time
	if req.Since != nil {
		since = *req.Since
	}

	var requeued int64
	err = h.DB.WithTenant(r.Context(), p.OrgID, func(ctx context.Context) error {
		tag, e := dbtx.Exec(ctx, h.DB, `
			INSERT INTO webhook_deliveries
			  (id, org_id, endpoint_id, event_type, event_id, payload, status, next_retry_at, attempt_count, max_attempts, created_at)
			SELECT gen_random_uuid(), org_id, endpoint_id, event_type, event_id, payload, 'pending', now(), 0, max_attempts, now()
			FROM webhook_deliveries
			WHERE endpoint_id = $1 AND org_id = $2
			  AND ($3 = '' OR event_type = $3)
			  AND ($4 = '' OR status = $4::webhook_status)
			  AND ($5::timestamptz IS NULL OR created_at >= $5)
			  AND NOT is_test
			ORDER BY created_at DESC
			LIMIT $6
		`, webhookID, p.OrgID, req.EventType, req.Status, nullableTime(since), limit)
		if e != nil {
			return e
		}
		requeued = tag.RowsAffected()
		return nil
	})
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "internal", "Failed to bulk replay")
		return
	}
	_ = h.Audit.Record(r.Context(), audit.Entry{
		Action: "webhook.deliver", ResourceType: "webhook", ResourceID: webhookID,
		Metadata: map[string]any{"bulk_replay": true, "requeued": requeued, "event_type": req.EventType, "status": req.Status},
	})
	writeJSON(w, http.StatusAccepted, map[string]any{"requeued": requeued})
}

// Enable handles POST /v1/webhooks/{id}/enable — re-activate an endpoint
// (e.g. after auto-disable) and reset its failure counter.
func (h *WebhookHandler) Enable(w http.ResponseWriter, r *http.Request) {
	p, err := auth.PrincipalFromContext(r.Context())
	if err != nil {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "Unauthorized")
		return
	}
	webhookID, ok := uuidParam(r, "id")
	if !ok {
		writeProblem(w, http.StatusNotFound, "not_found", "Webhook not found")
		return
	}
	var found bool
	err = h.DB.WithTenant(r.Context(), p.OrgID, func(ctx context.Context) error {
		tag, e := dbtx.Exec(ctx, h.DB, `
			UPDATE webhook_endpoints SET active = true, consecutive_failures = 0, updated_at = now()
			WHERE id = $1
		`, webhookID)
		if e != nil {
			return e
		}
		found = tag.RowsAffected() > 0
		return nil
	})
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "internal", "Failed to enable webhook")
		return
	}
	if !found {
		writeProblem(w, http.StatusNotFound, "not_found", "Webhook not found")
		return
	}
	_ = h.Audit.Record(r.Context(), audit.Entry{
		Action: "webhook.update", ResourceType: "webhook", ResourceID: webhookID,
		Metadata: map[string]any{"enabled": true},
	})
	writeJSON(w, http.StatusOK, map[string]any{"id": webhookID, "active": true})
}

// nullableTime returns nil for the zero time so a SQL timestamptz param can be
// NULL (the filter treats NULL as "no lower bound").
func nullableTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}
