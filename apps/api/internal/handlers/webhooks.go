// Package handlers — webhook subscription endpoints.
//
// The /v1/webhooks surface manages a per-org set of HTTPS endpoints
// that receive signed event deliveries. Two pieces of state live
// behind it: the `webhook_endpoints` row (URL, secret, event
// subscription list, active flag) and the `webhook_deliveries` log
// (one row per attempt, with retry/backoff state). The actual
// delivery loop lives in internal/webhooks; this file is the
// CRUD + replay surface over both tables.
//
// NOTE: Phase 1 deviations from the OpenAPI WebhookDelivery schema:
//
//  1. Status enum — the DB enum is
//     ('pending','delivering','delivered','failed','exhausted') but
//     the spec enum is ('pending','succeeded','failed','retrying').
//     The response uses the DB values verbatim for Phase 1; a future
//     migration reconciles them.
//
//  2. Column → field names — the DB uses `endpoint_id` while the
//     spec uses `webhook_id`; `response_status` is surfaced as
//     `last_status_code`; `next_retry_at` is intentionally omitted
//     from the response (it is internal retry state, not part of
//     the public spec).
//
//  3. `last_error` — the DB has no dedicated error column. The
//     response derives `last_error` from the first line of
//     `response_body` (truncated to 200 chars), which is the same
//     information we have at hand today.
//
//  4. `payload_url` — not modelled. The OpenAPI field would link
//     to a stored copy of the event payload; we do not yet
//     persist one and the field is omitted from the response.
package handlers

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/orpheus/api/internal/audit"
	"github.com/orpheus/api/internal/auth"
	"github.com/orpheus/api/internal/db"
	"github.com/orpheus/api/internal/dbtx"
	"github.com/orpheus/api/internal/ssrfguard"
)

// WebhookHandler bundles the dependencies the webhook endpoints need.
type WebhookHandler struct {
	DB    *db.DB
	Audit *audit.Recorder
}

// CreateWebhookRequest is the request body for POST /v1/webhooks.
// `SubscribedEvents` lists event types the endpoint wants to receive
// (or `["*"]` to receive all). `Secret` is optional; if empty the
// server generates a 32-byte random secret and returns it in the
// response. The cleartext secret is never re-displayed after the
// create call.
type CreateWebhookRequest struct {
	URL              string   `json:"url"`
	SubscribedEvents []string `json:"subscribed_events"`
	Description      string   `json:"description"`
	Secret           string   `json:"secret,omitempty"`
}

// UpdateWebhookRequest is the request body for PATCH /v1/webhooks/{id}.
// Only present fields are modified. `URL`, `Description`, and
// `SubscribedEvents` are nil-when-absent; `Active` is a *bool so
// "false" can be distinguished from "missing".
type UpdateWebhookRequest struct {
	URL              *string  `json:"url,omitempty"`
	SubscribedEvents []string `json:"subscribed_events,omitempty"`
	Description      *string  `json:"description,omitempty"`
	Active           *bool    `json:"active,omitempty"`
}

// WebhookEndpoint is the response shape for the webhook endpoints.
// The `Secret` field is populated exactly once on create; subsequent
// reads leave it empty.
type WebhookEndpoint struct {
	ID               string    `json:"id"`
	URL              string    `json:"url"`
	SubscribedEvents []string  `json:"subscribed_events"`
	Description      string    `json:"description"`
	Active           bool      `json:"active"`
	Secret           string    `json:"secret,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// WebhookEndpointList is a cursor-paginated list of webhook endpoints.
type WebhookEndpointList struct {
	Data       []WebhookEndpoint `json:"data"`
	HasMore    bool              `json:"has_more"`
	NextCursor string            `json:"next_cursor"`
}

// WebhookDelivery is the response shape for a single delivery row.
// Field names follow the OpenAPI spec; the mapping from DB columns
// is described in the package-level comment.
type WebhookDelivery struct {
	ID             string     `json:"id"`
	WebhookID      string     `json:"webhook_id"`
	EventID        string     `json:"event_id"`
	EventType      string     `json:"event_type"`
	Status         string     `json:"status"`
	AttemptCount   int        `json:"attempt_count"`
	LastAttemptAt  *time.Time `json:"last_attempt_at,omitempty"`
	LastStatusCode *int       `json:"last_status_code,omitempty"`
	LastError      *string    `json:"last_error,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
}

// WebhookDeliveryList is a cursor-paginated list of webhook deliveries.
type WebhookDeliveryList struct {
	Data       []WebhookDelivery `json:"data"`
	HasMore    bool              `json:"has_more"`
	NextCursor string            `json:"next_cursor"`
}

// allowedEvents is the catalogue of event types the API publishes in
// Phase 1. The set is duplicated from the OpenAPI WebhookEvent enum
// (handlers/openapi.json). Keep in sync. The "*" wildcard is also
// accepted by validation; it is not a publishable event type but a
// subscription marker.
var allowedEvents = map[string]struct{}{
	"job.queued":            {},
	"job.dead_letter":       {},
	"job.started":           {},
	"job.succeeded":         {},
	"job.failed":            {},
	"job.canceled":          {},
	"upload.completed":      {},
	"upload.failed":         {},
	"api_key.created":       {},
	"api_key.revoked":       {},
	"billing.period_closed": {},
	"*":                     {},
}

// Create handles POST /v1/webhooks. It validates the request, mints a
// 32-byte random secret when the caller did not supply one, inserts
// the row, audits, and returns the endpoint with the cleartext
// secret populated.
func (h *WebhookHandler) Create(w http.ResponseWriter, r *http.Request) {
	p, err := auth.PrincipalFromContext(r.Context())
	if err != nil {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "Unauthorized")
		return
	}

	var req CreateWebhookRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, "validation", "Invalid JSON")
		return
	}
	if msg := validateCreate(&req); msg != "" {
		writeProblem(w, http.StatusBadRequest, "validation", msg)
		return
	}

	secret := req.Secret
	if secret == "" {
		buf := make([]byte, 32)
		if _, err := rand.Read(buf); err != nil {
			writeProblem(w, http.StatusInternalServerError, "internal", "rand failed")
			return
		}
		secret = base64.RawURLEncoding.EncodeToString(buf)
	}

	id := uuid.NewString()
	now := time.Now()
	err = h.DB.WithTenant(r.Context(), p.OrgID, func(ctx context.Context) error {
		_, err := dbtx.Exec(ctx, h.DB, `
			INSERT INTO webhook_endpoints
			  (id, org_id, url, secret, description, subscribed_events, active, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, true, $7, $7)
		`, id, p.OrgID, req.URL, secret, req.Description, req.SubscribedEvents, now)
		return err
	})
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "internal", "Failed to create webhook")
		return
	}

	_ = h.Audit.Record(r.Context(), audit.Entry{
		Action:       "webhook.create",
		ResourceType: "webhook",
		ResourceID:   id,
		Metadata:     map[string]any{"url": req.URL, "subscribed_events": req.SubscribedEvents},
	})

	writeJSON(w, http.StatusCreated, WebhookEndpoint{
		ID:               id,
		URL:              req.URL,
		SubscribedEvents: req.SubscribedEvents,
		Description:      req.Description,
		Active:           true,
		Secret:           secret,
		CreatedAt:        now,
		UpdatedAt:        now,
	})
}

// List handles GET /v1/webhooks. Cursor pagination is over (created_at).
// Secrets are never returned.
func (h *WebhookHandler) List(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFromContext(r.Context())
	limit := 50
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 && n <= 200 {
			limit = n
		}
	}
	cursor := r.URL.Query().Get("cursor")

	args := []any{p.OrgID}
	query := `SELECT id::text, url, description, subscribed_events, active, created_at, updated_at
	          FROM webhook_endpoints WHERE org_id = $1`
	argIdx := 2
	if cursor != "" {
		query += fmt.Sprintf(" AND created_at < $%d", argIdx)
		args = append(args, cursor)
		argIdx++
	}
	query += fmt.Sprintf(" ORDER BY created_at DESC LIMIT $%d", argIdx)
	args = append(args, limit+1)

	var eps []WebhookEndpoint
	err := h.DB.WithTenant(r.Context(), p.OrgID, func(ctx context.Context) error {
		rows, err := dbtx.Query(ctx, h.DB, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e WebhookEndpoint
			if err := rows.Scan(&e.ID, &e.URL, &e.Description, &e.SubscribedEvents, &e.Active, &e.CreatedAt, &e.UpdatedAt); err != nil {
				return err
			}
			eps = append(eps, e)
		}
		return rows.Err()
	})
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "internal", "Failed to list webhooks")
		return
	}

	hasMore := len(eps) > limit
	if hasMore {
		eps = eps[:limit]
	}
	nextCursor := ""
	if hasMore && len(eps) > 0 {
		nextCursor = eps[len(eps)-1].CreatedAt.Format(time.RFC3339Nano)
	}

	writeJSON(w, http.StatusOK, WebhookEndpointList{Data: eps, HasMore: hasMore, NextCursor: nextCursor})
}

// Get handles GET /v1/webhooks/{id}. 404 when the row is not visible
// to the caller's org.
func (h *WebhookHandler) Get(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFromContext(r.Context())
	id, ok := uuidParam(r, "id")
	if !ok {
		writeProblem(w, http.StatusNotFound, "not_found", "Webhook not found")
		return
	}

	var e WebhookEndpoint
	err := h.DB.WithTenant(r.Context(), p.OrgID, func(ctx context.Context) error {
		return dbtx.QueryRow(ctx, h.DB, `
			SELECT id::text, url, description, subscribed_events, active, created_at, updated_at
			FROM webhook_endpoints WHERE id = $1
		`, id).Scan(&e.ID, &e.URL, &e.Description, &e.SubscribedEvents, &e.Active, &e.CreatedAt, &e.UpdatedAt)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeProblem(w, http.StatusNotFound, "not_found", "Webhook not found")
			return
		}
		writeProblem(w, http.StatusInternalServerError, "internal", "Failed to get webhook")
		return
	}
	writeJSON(w, http.StatusOK, e)
}

// Update handles PATCH /v1/webhooks/{id}. Only fields present in the
// request body are modified. `Active` is a *bool so explicit `false`
// is honoured.
func (h *WebhookHandler) Update(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFromContext(r.Context())
	id, ok := uuidParam(r, "id")
	if !ok {
		writeProblem(w, http.StatusNotFound, "not_found", "Webhook not found")
		return
	}

	var req UpdateWebhookRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, "validation", "Invalid JSON")
		return
	}
	if req.URL == nil && req.SubscribedEvents == nil && req.Description == nil && req.Active == nil {
		writeProblem(w, http.StatusBadRequest, "validation", "no fields to update")
		return
	}
	if msg := validateUpdate(&req); msg != "" {
		writeProblem(w, http.StatusBadRequest, "validation", msg)
		return
	}

	now := time.Now()
	var updated WebhookEndpoint
	err := h.DB.WithTenant(r.Context(), p.OrgID, func(ctx context.Context) error {
		// Dynamic SET clause: each present field gets its own
		// placeholder, and the arg list grows alongside.
		set := "updated_at = $2"
		args := []any{id, now}
		idx := 3
		if req.URL != nil {
			set += fmt.Sprintf(", url = $%d", idx)
			args = append(args, *req.URL)
			idx++
		}
		if req.SubscribedEvents != nil {
			set += fmt.Sprintf(", subscribed_events = $%d", idx)
			args = append(args, req.SubscribedEvents)
			idx++
		}
		if req.Description != nil {
			set += fmt.Sprintf(", description = $%d", idx)
			args = append(args, *req.Description)
			idx++
		}
		if req.Active != nil {
			set += fmt.Sprintf(", active = $%d", idx)
			args = append(args, *req.Active)
		}
		query := fmt.Sprintf(`
			UPDATE webhook_endpoints SET %s WHERE id = $1
			RETURNING id::text, url, description, subscribed_events, active, created_at, updated_at
		`, set)
		return dbtx.QueryRow(ctx, h.DB, query, args...).Scan(
			&updated.ID, &updated.URL, &updated.Description, &updated.SubscribedEvents,
			&updated.Active, &updated.CreatedAt, &updated.UpdatedAt,
		)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeProblem(w, http.StatusNotFound, "not_found", "Webhook not found")
			return
		}
		writeProblem(w, http.StatusInternalServerError, "internal", "Failed to update webhook")
		return
	}

	_ = h.Audit.Record(r.Context(), audit.Entry{
		Action:       "webhook.update",
		ResourceType: "webhook",
		ResourceID:   id,
	})

	writeJSON(w, http.StatusOK, updated)
}

// Delete handles DELETE /v1/webhooks/{id}. Idempotent: missing rows
// return 204 just like a real delete.
func (h *WebhookHandler) Delete(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFromContext(r.Context())
	id, ok := uuidParam(r, "id")
	if !ok {
		// Delete is idempotent: a malformed/unknown id is a no-op 204.
		w.WriteHeader(http.StatusNoContent)
		return
	}

	err := h.DB.WithTenant(r.Context(), p.OrgID, func(ctx context.Context) error {
		_, err := dbtx.Exec(ctx, h.DB, `DELETE FROM webhook_endpoints WHERE id = $1`, id)
		return err
	})
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "internal", "Failed to delete webhook")
		return
	}

	_ = h.Audit.Record(r.Context(), audit.Entry{
		Action:       "webhook.delete",
		ResourceType: "webhook",
		ResourceID:   id,
	})

	w.WriteHeader(http.StatusNoContent)
}

// ListDeliveries handles GET /v1/webhooks/{id}/deliveries. Filters:
// `event_type`, `status`, `limit` (1..200, default 50; values above
// 200 are silently capped), `cursor` (created_at RFC3339Nano).
func (h *WebhookHandler) ListDeliveries(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFromContext(r.Context())
	id, ok := uuidParam(r, "id")
	if !ok {
		writeProblem(w, http.StatusNotFound, "not_found", "Webhook not found")
		return
	}

	limit := 50
	if l := r.URL.Query().Get("limit"); l != "" {
		n, err := strconv.Atoi(l)
		if err != nil || n < 1 {
			writeProblem(w, http.StatusBadRequest, "validation", "limit must be a positive integer")
			return
		}
		// Phase 1 decision: cap silently rather than 400. The OpenAPI
		// spec does not specify; capping matches the rest of the
		// list endpoints in the surface.
		if n > 200 {
			n = 200
		}
		limit = n
	}
	cursor := r.URL.Query().Get("cursor")
	eventType := r.URL.Query().Get("event_type")
	statusFilter := r.URL.Query().Get("status")
	if statusFilter != "" {
		if !validDeliveryStatus(statusFilter) {
			writeProblem(w, http.StatusBadRequest, "validation", "invalid status")
			return
		}
	}

	var exists bool
	err := h.DB.WithTenant(r.Context(), p.OrgID, func(ctx context.Context) error {
		return dbtx.QueryRow(ctx, h.DB, `SELECT EXISTS(SELECT 1 FROM webhook_endpoints WHERE id = $1 AND org_id = $2)`, id, p.OrgID).Scan(&exists)
	})
	if err != nil || !exists {
		writeProblem(w, http.StatusNotFound, "not_found", "Webhook not found")
		return
	}

	args := []any{id, p.OrgID}
	query := `SELECT id::text, endpoint_id::text, event_id::text, event_type,
	                 response_status, response_body, attempt_count, status, delivered_at, created_at
	          FROM webhook_deliveries WHERE endpoint_id = $1 AND org_id = $2`
	argIdx := 2
	if cursor != "" {
		query += fmt.Sprintf(" AND created_at < $%d", argIdx)
		args = append(args, cursor)
		argIdx++
	}
	if eventType != "" {
		query += fmt.Sprintf(" AND event_type = $%d", argIdx)
		args = append(args, eventType)
		argIdx++
	}
	if statusFilter != "" {
		query += fmt.Sprintf(" AND status = $%d::webhook_status", argIdx)
		args = append(args, statusFilter)
		argIdx++
	}
	query += fmt.Sprintf(" ORDER BY created_at DESC LIMIT $%d", argIdx)
	args = append(args, limit+1)

	var deliveries []WebhookDelivery
	err = h.DB.WithTenant(r.Context(), p.OrgID, func(ctx context.Context) error {
		rows, err := dbtx.Query(ctx, h.DB, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var (
				d           WebhookDelivery
				deliveredAt *time.Time
				respStatus  *int
				respBody    *string
				dbStatus    string
			)
			if err := rows.Scan(&d.ID, &d.WebhookID, &d.EventID, &d.EventType, &respStatus, &respBody, &d.AttemptCount, &dbStatus, &deliveredAt, &d.CreatedAt); err != nil {
				return err
			}
			d.Status = dbStatus
			d.LastStatusCode = respStatus
			if deliveredAt != nil {
				d.LastAttemptAt = deliveredAt
			} else {
				t := d.CreatedAt
				d.LastAttemptAt = &t
			}
			if respBody != nil && *respBody != "" {
				first := firstLine(*respBody, 200)
				d.LastError = &first
			}
			deliveries = append(deliveries, d)
		}
		return rows.Err()
	})
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "internal", "Failed to list deliveries")
		return
	}

	hasMore := len(deliveries) > limit
	if hasMore {
		deliveries = deliveries[:limit]
	}
	nextCursor := ""
	if hasMore && len(deliveries) > 0 {
		nextCursor = deliveries[len(deliveries)-1].CreatedAt.Format(time.RFC3339Nano)
	}

	writeJSON(w, http.StatusOK, WebhookDeliveryList{Data: deliveries, HasMore: hasMore, NextCursor: nextCursor})
}

// Replay handles POST /v1/webhooks/{id}/deliveries/{delivery_id}/replay.
// A fresh delivery row is inserted with attempt_count=0 and
// status=pending; the source row is preserved for audit.
func (h *WebhookHandler) Replay(w http.ResponseWriter, r *http.Request) {
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
	deliveryID, ok := uuidParam(r, "delivery_id")
	if !ok {
		writeProblem(w, http.StatusNotFound, "not_found", "Delivery not found")
		return
	}

	newID := uuid.NewString()
	now := time.Now()
	var out WebhookDelivery
	err = h.DB.WithTenant(r.Context(), p.OrgID, func(ctx context.Context) error {
		// Pull the source row first. RLS scopes the read to the
		// caller's org through the WithTenant tx on ctx.
		var (
			srcEventType, srcEventID, dbStatus string
		)
		if err := dbtx.QueryRow(ctx, h.DB, `
			SELECT event_type, event_id::text, status::text
			FROM webhook_deliveries
			WHERE id = $1 AND endpoint_id = $2 AND org_id = $3
		`, deliveryID, webhookID, p.OrgID).Scan(&srcEventType, &srcEventID, &dbStatus); err != nil {
			return err
		}

		if _, err := dbtx.Exec(ctx, h.DB, `
			INSERT INTO webhook_deliveries
			  (id, org_id, endpoint_id, event_type, event_id, payload, status, next_retry_at, attempt_count, max_attempts, created_at)
			SELECT $1, $2, endpoint_id, event_type, event_id, payload, 'pending', now(), 0, max_attempts, $3
			FROM webhook_deliveries
			WHERE id = $4 AND endpoint_id = $5 AND org_id = $2
		`, newID, p.OrgID, now, deliveryID, webhookID); err != nil {
			return err
		}

		out = WebhookDelivery{
			ID:            newID,
			WebhookID:     webhookID,
			EventID:       srcEventID,
			EventType:     srcEventType,
			Status:        "pending",
			AttemptCount:  0,
			CreatedAt:     now,
			LastAttemptAt: &now,
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeProblem(w, http.StatusNotFound, "not_found", "Delivery not found")
			return
		}
		writeProblem(w, http.StatusInternalServerError, "internal", "Failed to replay delivery")
		return
	}

	_ = h.Audit.Record(r.Context(), audit.Entry{
		Action:       "webhook.deliver",
		ResourceType: "webhook",
		ResourceID:   webhookID,
		Metadata:     map[string]any{"source_delivery_id": deliveryID, "new_delivery_id": newID, "replay": true},
	})

	writeJSON(w, http.StatusAccepted, out)
}

// validateWebhookURL returns "" if url is a permitted public https
// target, or a reason string otherwise. It rejects internal/metadata
// targets (SSRF): loopback, RFC1918/ULA, link-local (incl.
// 169.254.169.254), and hosts that resolve into those ranges. The
// delivery client re-checks at dial time to close the DNS-rebind window.
func validateWebhookURL(raw string) string {
	if err := ssrfguard.ValidateURLStatic(raw); err != nil {
		var dis *ssrfguard.ErrDisallowed
		if errors.As(err, &dis) {
			return dis.Reason
		}
		return "url invalid"
	}
	return ""
}

// validateCreate returns "" on success, or a human-readable reason
// string for the 400 response.
func validateCreate(req *CreateWebhookRequest) string {
	if req.URL == "" {
		return "url required"
	}
	if err := validateWebhookURL(req.URL); err != "" {
		return err
	}
	if len(req.SubscribedEvents) == 0 {
		return "subscribed_events required"
	}
	for _, e := range req.SubscribedEvents {
		if _, ok := allowedEvents[e]; !ok {
			return "invalid event type: " + e
		}
	}
	if req.Secret != "" {
		if len(req.Secret) < 16 || len(req.Secret) > 256 {
			return "secret must be 16..256 chars"
		}
	}
	return ""
}

func validateUpdate(req *UpdateWebhookRequest) string {
	if req.URL != nil {
		if err := validateWebhookURL(*req.URL); err != "" {
			return err
		}
	}
	if req.SubscribedEvents != nil {
		if len(req.SubscribedEvents) == 0 {
			return "subscribed_events must be non-empty when present"
		}
		for _, e := range req.SubscribedEvents {
			if _, ok := allowedEvents[e]; !ok {
				return "invalid event type: " + e
			}
		}
	}
	return ""
}

func validDeliveryStatus(s string) bool {
	switch s {
	case "pending", "delivering", "delivered", "failed", "exhausted":
		return true
	}
	return false
}

func firstLine(s string, max int) string {
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' || i == max {
			if i > max {
				i = max
			}
			return s[:i]
		}
	}
	if len(s) > max {
		return s[:max]
	}
	return s
}
