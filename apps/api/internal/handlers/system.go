// Package handlers — org-scoped system endpoints (usage and audit log).
//
// Both endpoints are tenant-scoped reads: they pull every row through
// the WithTenant helper so RLS scopes the query to the caller's org.
// No writes happen here, so neither endpoint needs the audit
// recorder; the audit log endpoint is the *source* of audit rows
// rather than a producer of new ones.
package handlers

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/orpheus/api/internal/audit"
	"github.com/orpheus/api/internal/auth"
	"github.com/orpheus/api/internal/db"
	"github.com/orpheus/api/internal/dbtx"
)

// SystemHandler bundles the dependencies the system endpoints need.
// Both endpoints are reads today; the DB pool alone is enough. Once
// Phase 1+ adds admin-only writes (usage reconciliation, audit log
// redaction) the audit.Recorder can be added without changing the
// router wiring.
type SystemHandler struct {
	DB *db.DB
}

// Usage is the response shape for GET /v1/usage. It is a flat
// org-aggregate for the current billing period; per-category
// breakdown will be added once the billing pipeline (#102) lands.
type Usage struct {
	JobsCount     int     `json:"jobs_count"`
	GPUSeconds    float64 `json:"gpu_seconds"`
	StorageGBDays float64 `json:"storage_gb_days"`
	EgressGB      float64 `json:"egress_gb"`
	TotalUSD      float64 `json:"total_usd"`
}

// AuditLogEntry is a single audit_log row projected for the wire.
// The internal actor_type enum (`user`, `apikey`, `system`) is
// returned as-is; consumers that need a UI label can switch on the
// value client-side.
type AuditLogEntry struct {
	ID           string    `json:"id"`
	ActorID      string    `json:"actor_id,omitempty"`
	ActorType    string    `json:"actor_type,omitempty"`
	Action       string    `json:"action"`
	ResourceType string    `json:"resource_type,omitempty"`
	ResourceID   string    `json:"resource_id,omitempty"`
	IP           string    `json:"ip,omitempty"`
	UserAgent    string    `json:"user_agent,omitempty"`
	RequestID    string    `json:"request_id,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
}

// AuditLogList is a cursor-paginated list of audit log entries.
type AuditLogList struct {
	Data       []AuditLogEntry `json:"data"`
	HasMore    bool            `json:"has_more"`
	NextCursor string          `json:"next_cursor"`
}

// GetUsage handles GET /v1/usage. The current month is the default
// period; Phase 1+ will honour the `period` query param (an ISO
// month like `2026-07`) once the billing pipeline is wired in.
//
// We derive usage from the `jobs` table today: jobs_count is the
// number of jobs submitted in the window; gpu_seconds is the wall
// clock from started_at to completed_at for terminal jobs. storage
// and egress are not tracked yet and come back as 0.
func (h *SystemHandler) GetUsage(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFromContext(r.Context())

	var u Usage
	err := h.DB.WithTenant(r.Context(), p.OrgID, func(ctx context.Context) error {
		return dbtx.QueryRow(ctx, h.DB, `
			SELECT
				COUNT(*)::int,
				COALESCE(SUM(EXTRACT(EPOCH FROM (completed_at - started_at))), 0)::float8,
				0::float8,
				0::float8,
				COALESCE(SUM(cost_usd), 0)::float8
			FROM jobs
			WHERE org_id = $1
			  AND created_at >= date_trunc('month', now())
		`, p.OrgID).Scan(
			&u.JobsCount,
			&u.GPUSeconds,
			&u.StorageGBDays,
			&u.EgressGB,
			&u.TotalUSD,
		)
	})
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "internal", "Failed to get usage")
		return
	}

	writeJSON(w, http.StatusOK, u)
}

// ListAuditLog handles GET /v1/audit-log. Filters: action, actor_id,
// resource_type, and the optional `cursor` / `created_after` /
// `created_before` for time-range pagination. The cursor is the
// created_at timestamp of the last item on the previous page.
func (h *SystemHandler) ListAuditLog(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFromContext(r.Context())

	limit := 50
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 && n <= 200 {
			limit = n
		}
	}
	cursor := r.URL.Query().Get("cursor")
	action := r.URL.Query().Get("action")
	actorID := r.URL.Query().Get("actor_id")
	resourceType := r.URL.Query().Get("resource_type")
	createdAfter := r.URL.Query().Get("created_after")
	createdBefore := r.URL.Query().Get("created_before")

	// Validate typed params before they hit SQL enum/uuid/timestamp casts
	// so bad client input is a 400, not a 500 leaking a DB error.
	if !validCursor(cursor) || !validCursor(createdAfter) || !validCursor(createdBefore) {
		writeProblem(w, http.StatusBadRequest, "validation", "invalid timestamp/cursor")
		return
	}
	if action != "" && !audit.IsValidAction(action) {
		writeProblem(w, http.StatusBadRequest, "validation", "invalid action")
		return
	}
	if actorID != "" {
		if _, err := uuid.Parse(actorID); err != nil {
			writeProblem(w, http.StatusBadRequest, "validation", "invalid actor_id")
			return
		}
	}

	args := []any{p.OrgID}
	where := "WHERE org_id = $1"
	argIdx := 2
	if cursor != "" {
		where += fmt.Sprintf(" AND created_at < $%d", argIdx)
		args = append(args, cursor)
		argIdx++
	}
	if action != "" {
		where += fmt.Sprintf(" AND action = $%d::audit_action", argIdx)
		args = append(args, action)
		argIdx++
	}
	if actorID != "" {
		where += fmt.Sprintf(" AND user_id = $%d::uuid", argIdx)
		args = append(args, actorID)
		argIdx++
	}
	if resourceType != "" {
		where += fmt.Sprintf(" AND resource_type = $%d", argIdx)
		args = append(args, resourceType)
		argIdx++
	}
	if createdAfter != "" {
		where += fmt.Sprintf(" AND created_at >= $%d", argIdx)
		args = append(args, createdAfter)
		argIdx++
	}
	if createdBefore != "" {
		where += fmt.Sprintf(" AND created_at <= $%d", argIdx)
		args = append(args, createdBefore)
		argIdx++
	}
	args = append(args, limit+1)
	query := fmt.Sprintf(`
		SELECT
			id,
			COALESCE(user_id::text, ''),
			actor_type::text,
			action::text,
			COALESCE(resource_type, ''),
			COALESCE(resource_id, ''),
			COALESCE(host(ip), ''),
			COALESCE(user_agent, ''),
			COALESCE(request_id, ''),
			created_at
		FROM audit_log
		%s
		ORDER BY created_at DESC
		LIMIT $%d
	`, where, argIdx)

	var entries []AuditLogEntry
	err := h.DB.WithTenant(r.Context(), p.OrgID, func(ctx context.Context) error {
		rows, err := dbtx.Query(ctx, h.DB, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var (
				e                               AuditLogEntry
				actorID, actorType, resType     string
				resID, ip, userAgent, requestID string
			)
			if err := rows.Scan(
				&e.ID,
				&actorID,
				&actorType,
				&e.Action,
				&resType,
				&resID,
				&ip,
				&userAgent,
				&requestID,
				&e.CreatedAt,
			); err != nil {
				return err
			}
			e.ActorID = actorID
			e.ActorType = actorType
			e.ResourceType = resType
			e.ResourceID = resID
			e.IP = ip
			e.UserAgent = userAgent
			e.RequestID = requestID
			entries = append(entries, e)
		}
		return rows.Err()
	})
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "internal", "Failed to list audit log")
		return
	}

	hasMore := len(entries) > limit
	if hasMore {
		entries = entries[:limit]
	}
	nextCursor := ""
	if hasMore && len(entries) > 0 {
		nextCursor = entries[len(entries)-1].CreatedAt.Format(time.RFC3339Nano)
	}

	writeJSON(w, http.StatusOK, AuditLogList{Data: entries, HasMore: hasMore, NextCursor: nextCursor})
}
