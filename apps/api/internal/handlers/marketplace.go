// Package handlers — processor marketplace endpoints (Phase 7).
//
// The marketplace exposes the processor catalog with its trust classification
// (first_party / verified / community) and a moderation queue: a tenant submits
// a community processor, an admin approves it (promoting it into the public
// catalog) or rejects it. First-party processors are synced from code
// (catalog sync); this surface governs third-party/community ones.
package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/orpheus/api/internal/auth"
	"github.com/orpheus/api/internal/db"
	"github.com/orpheus/api/internal/dbtx"
)

type MarketplaceHandler struct {
	DB *db.DB
}

type MarketplaceProcessor struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	Description string `json:"description"`
	TrustClass  string `json:"trust_class"`
	Publisher   string `json:"publisher"`
	Tier        string `json:"tier"`
}

type MarketplaceSubmission struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	DisplayName string     `json:"display_name"`
	Description string     `json:"description"`
	Publisher   string     `json:"publisher"`
	Status      string     `json:"status"`
	ReviewNotes string     `json:"review_notes,omitempty"`
	SubmittedAt time.Time  `json:"submitted_at"`
	ReviewedAt  *time.Time `json:"reviewed_at,omitempty"`
}

// ListProcessors returns the public catalog with trust metadata. Optional
// ?trust_class= filter.
func (h *MarketplaceHandler) ListProcessors(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFromContext(r.Context())
	trust := r.URL.Query().Get("trust_class")
	out := []MarketplaceProcessor{}
	err := h.DB.WithTenant(r.Context(), p.OrgID, func(ctx context.Context) error {
		rows, err := dbtx.Query(ctx, h.DB,
			`SELECT name, display_name, description, trust_class, publisher, tier::text
			 FROM processors
			 WHERE ($1 = '' OR trust_class = $1)
			 ORDER BY trust_class, name`, trust)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var m MarketplaceProcessor
			if err := rows.Scan(&m.Name, &m.DisplayName, &m.Description, &m.TrustClass, &m.Publisher, &m.Tier); err != nil {
				return err
			}
			out = append(out, m)
		}
		return rows.Err()
	})
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "internal", "Failed to list processors")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

type submitRequest struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	Description string `json:"description"`
	Publisher   string `json:"publisher"`
}

// Submit queues a community processor for moderation.
func (h *MarketplaceHandler) Submit(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFromContext(r.Context())
	var req submitRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, "validation", "Invalid JSON")
		return
	}
	if req.Name == "" || req.DisplayName == "" || req.Publisher == "" {
		writeProblem(w, http.StatusBadRequest, "validation", "name, display_name, publisher required")
		return
	}
	var id string
	err := h.DB.WithTenant(r.Context(), p.OrgID, func(ctx context.Context) error {
		return dbtx.QueryRow(ctx, h.DB,
			`INSERT INTO marketplace_submissions (org_id, name, display_name, description, publisher)
			 VALUES ($1, $2, $3, $4, $5) RETURNING id`,
			p.OrgID, req.Name, req.DisplayName, req.Description, req.Publisher,
		).Scan(&id)
	})
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "internal", "Failed to submit")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": id, "status": "pending"})
}

// ListSubmissions returns the caller org's submissions.
func (h *MarketplaceHandler) ListSubmissions(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFromContext(r.Context())
	out := []MarketplaceSubmission{}
	err := h.DB.WithTenant(r.Context(), p.OrgID, func(ctx context.Context) error {
		rows, err := dbtx.Query(ctx, h.DB,
			`SELECT id, name, display_name, description, publisher, status, COALESCE(review_notes,''), submitted_at, reviewed_at
			 FROM marketplace_submissions WHERE org_id = $1 ORDER BY submitted_at DESC LIMIT 100`, p.OrgID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var s MarketplaceSubmission
			if err := rows.Scan(&s.ID, &s.Name, &s.DisplayName, &s.Description, &s.Publisher,
				&s.Status, &s.ReviewNotes, &s.SubmittedAt, &s.ReviewedAt); err != nil {
				return err
			}
			out = append(out, s)
		}
		return rows.Err()
	})
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "internal", "Failed to list submissions")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

type reviewRequest struct {
	Decision string `json:"decision"` // "approve" | "reject"
	Notes    string `json:"notes,omitempty"`
}

// Review approves or rejects a submission. Approval promotes the processor into
// the public catalog as a community-trust processor. Requires the "*" scope
// (moderation is an admin action). Runs service-role so it can cross tenants
// and write the catalog.
func (h *MarketplaceHandler) Review(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req reviewRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, "validation", "Invalid JSON")
		return
	}
	if req.Decision != "approve" && req.Decision != "reject" {
		writeProblem(w, http.StatusBadRequest, "validation", "decision must be approve or reject")
		return
	}
	newStatus := "approved"
	if req.Decision == "reject" {
		newStatus = "rejected"
	}

	var notFound, alreadyReviewed bool
	err := h.withServiceTx(r.Context(), func(tx pgx.Tx) error {
		var name, displayName, description, publisher, curStatus string
		e := tx.QueryRow(r.Context(),
			`SELECT name, display_name, description, publisher, status FROM marketplace_submissions WHERE id = $1`, id,
		).Scan(&name, &displayName, &description, &publisher, &curStatus)
		if errors.Is(e, pgx.ErrNoRows) {
			notFound = true
			return nil
		}
		if e != nil {
			return e
		}
		if curStatus != "pending" {
			alreadyReviewed = true
			return nil
		}
		if _, e := tx.Exec(r.Context(),
			`UPDATE marketplace_submissions SET status = $2, review_notes = $3, reviewed_at = now() WHERE id = $1`,
			id, newStatus, req.Notes); e != nil {
			return e
		}
		if newStatus == "approved" {
			// Promote into the public catalog as a community processor.
			if _, e := tx.Exec(r.Context(),
				`INSERT INTO processors (name, display_name, description, tier, timeout_seconds, trust_class, publisher)
				 VALUES ($1, $2, $3, 'cpu_tiny', 300, 'community', $4)
				 ON CONFLICT (name) DO UPDATE SET
				    display_name = EXCLUDED.display_name, description = EXCLUDED.description,
				    trust_class = 'community', publisher = EXCLUDED.publisher`,
				name, displayName, description, publisher); e != nil {
				return e
			}
		}
		return nil
	})
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "internal", "Failed to review")
		return
	}
	if notFound {
		writeProblem(w, http.StatusNotFound, "not_found", "Submission not found")
		return
	}
	if alreadyReviewed {
		writeProblem(w, http.StatusConflict, "conflict", "Submission already reviewed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "status": newStatus})
}

func (h *MarketplaceHandler) withServiceTx(ctx context.Context, fn func(pgx.Tx) error) error {
	conn, err := h.DB.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()
	if _, err := tx.Exec(ctx, "SELECT set_config('app.is_service','true',true)"); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	committed = true
	return nil
}
