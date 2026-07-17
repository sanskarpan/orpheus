// Package handlers — spend budgets + threshold alerts (PRD 07).
package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/orpheus/api/internal/audit"
	"github.com/orpheus/api/internal/auth"
	"github.com/orpheus/api/internal/db"
	"github.com/orpheus/api/internal/dbtx"
)

// BudgetHandler serves the budget endpoints.
type BudgetHandler struct {
	DB    *db.DB
	Audit *audit.Recorder
}

// BudgetRequest is the body for POST/PATCH /v1/budgets.
type BudgetRequest struct {
	Scope           string    `json:"scope"`
	ScopeID         string    `json:"scope_id"`
	Period          string    `json:"period"`
	LimitUSD        float64   `json:"limit_usd"`
	AlertThresholds []float64 `json:"alert_thresholds"`
	Enforcement     string    `json:"enforcement"`
}

// BudgetView is the wire shape of a budget with current spend.
type BudgetView struct {
	ID              string    `json:"id"`
	Scope           string    `json:"scope"`
	ScopeID         string    `json:"scope_id,omitempty"`
	Period          string    `json:"period"`
	LimitUSD        float64   `json:"limit_usd"`
	AlertThresholds []float64 `json:"alert_thresholds"`
	Enforcement     string    `json:"enforcement"`
	SpendUSD        float64   `json:"spend_usd"`
	PercentConsumed float64   `json:"percent_consumed"`
	CreatedAt       time.Time `json:"created_at"`
}

// Create handles POST /v1/budgets.
func (h *BudgetHandler) Create(w http.ResponseWriter, r *http.Request) {
	p, err := auth.PrincipalFromContext(r.Context())
	if err != nil {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "Unauthorized")
		return
	}
	var req BudgetRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, "validation", "Invalid JSON")
		return
	}
	if req.Scope == "" {
		req.Scope = "org"
	}
	if req.Scope != "org" && req.Scope != "processor" {
		writeProblem(w, http.StatusBadRequest, "validation", "scope must be org or processor")
		return
	}
	if req.Scope == "processor" && req.ScopeID == "" {
		writeProblem(w, http.StatusBadRequest, "validation", "scope_id (processor name) required for scope=processor")
		return
	}
	if req.Period == "" {
		req.Period = "monthly"
	}
	if req.LimitUSD <= 0 {
		writeProblem(w, http.StatusBadRequest, "validation", "limit_usd must be > 0")
		return
	}
	if req.Enforcement == "" {
		req.Enforcement = "alert"
	}
	if req.Enforcement != "alert" && req.Enforcement != "hard_cap" {
		writeProblem(w, http.StatusBadRequest, "validation", "enforcement must be alert or hard_cap")
		return
	}
	if len(req.AlertThresholds) == 0 {
		req.AlertThresholds = []float64{0.5, 0.8, 1.0}
	}

	id := uuid.NewString()
	now := time.Now()
	err = h.DB.WithTenant(r.Context(), p.OrgID, func(ctx context.Context) error {
		_, e := dbtx.Exec(ctx, h.DB, `
			INSERT INTO budgets (id, org_id, scope, scope_id, period, limit_usd, alert_thresholds, enforcement, created_at, updated_at)
			VALUES ($1,$2,$3,NULLIF($4,''),$5,$6,$7,$8,$9,$9)
		`, id, p.OrgID, req.Scope, req.ScopeID, req.Period, req.LimitUSD, req.AlertThresholds, req.Enforcement, now)
		return e
	})
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "internal", "Failed to create budget")
		return
	}
	if h.Audit != nil {
		_ = h.Audit.Record(r.Context(), audit.Entry{Action: "billing.plan_change", ResourceType: "budget", ResourceID: id,
			Metadata: map[string]any{"limit_usd": req.LimitUSD, "enforcement": req.Enforcement}})
	}
	writeJSON(w, http.StatusCreated, BudgetView{
		ID: id, Scope: req.Scope, ScopeID: req.ScopeID, Period: req.Period, LimitUSD: req.LimitUSD,
		AlertThresholds: req.AlertThresholds, Enforcement: req.Enforcement, CreatedAt: now,
	})
}

// List handles GET /v1/budgets — budgets with current-period spend + % consumed.
func (h *BudgetHandler) List(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFromContext(r.Context())
	var out []BudgetView
	err := h.DB.WithTenant(r.Context(), p.OrgID, func(ctx context.Context) error {
		rows, e := dbtx.Query(ctx, h.DB, `
			SELECT b.id, b.scope, COALESCE(b.scope_id,''), b.period, b.limit_usd::float8, b.alert_thresholds, b.enforcement, b.created_at,
			       COALESCE((
			         SELECT sum(cost_usd) FROM usage_rollup_hourly u
			         WHERE u.org_id=b.org_id AND u.hour >= date_trunc('month', now())
			           AND ((b.scope='org' AND u.dimension='total')
			             OR (b.scope='processor' AND u.dimension='processor' AND u.dimension_value=b.scope_id))
			       ),0)::float8 AS spend
			FROM budgets b WHERE b.org_id=$1 ORDER BY b.created_at DESC
		`, p.OrgID)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var b BudgetView
			if e := rows.Scan(&b.ID, &b.Scope, &b.ScopeID, &b.Period, &b.LimitUSD, &b.AlertThresholds, &b.Enforcement, &b.CreatedAt, &b.SpendUSD); e != nil {
				return e
			}
			if b.LimitUSD > 0 {
				b.PercentConsumed = b.SpendUSD / b.LimitUSD
			}
			out = append(out, b)
		}
		return rows.Err()
	})
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "internal", "Failed to list budgets")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

// Update handles PATCH /v1/budgets/{id}.
func (h *BudgetHandler) Update(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFromContext(r.Context())
	id, ok := uuidParam(r, "id")
	if !ok {
		writeProblem(w, http.StatusNotFound, "not_found", "Budget not found")
		return
	}
	var req BudgetRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, "validation", "Invalid JSON")
		return
	}
	var found bool
	err := h.DB.WithTenant(r.Context(), p.OrgID, func(ctx context.Context) error {
		tag, e := dbtx.Exec(ctx, h.DB, `
			UPDATE budgets SET
			  limit_usd = COALESCE(NULLIF($2,0), limit_usd),
			  enforcement = COALESCE(NULLIF($3,''), enforcement),
			  alert_thresholds = COALESCE($4, alert_thresholds),
			  updated_at = now()
			WHERE id=$1
		`, id, req.LimitUSD, req.Enforcement, nullableFloatArray(req.AlertThresholds))
		if e != nil {
			return e
		}
		found = tag.RowsAffected() > 0
		return nil
	})
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "internal", "Failed to update budget")
		return
	}
	if !found {
		writeProblem(w, http.StatusNotFound, "not_found", "Budget not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "updated": true})
}

// Delete handles DELETE /v1/budgets/{id}.
func (h *BudgetHandler) Delete(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFromContext(r.Context())
	id, ok := uuidParam(r, "id")
	if !ok {
		writeProblem(w, http.StatusNotFound, "not_found", "Budget not found")
		return
	}
	var n int64
	err := h.DB.WithTenant(r.Context(), p.OrgID, func(ctx context.Context) error {
		tag, e := dbtx.Exec(ctx, h.DB, `DELETE FROM budgets WHERE id=$1`, id)
		if e != nil {
			return e
		}
		n = tag.RowsAffected()
		return nil
	})
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "internal", "Failed to delete budget")
		return
	}
	if n == 0 {
		writeProblem(w, http.StatusNotFound, "not_found", "Budget not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// nullableFloatArray returns nil for an empty slice so a COALESCE keeps the
// existing value in PATCH.
func nullableFloatArray(f []float64) any {
	if len(f) == 0 {
		return nil
	}
	return f
}

// budgetHardCapExceeded reports whether a hard_cap budget for the org (org
// scope, or processor scope matching processorName) is already at/over limit.
// Best-effort at submit time (rollup lag ≤ minutes) per the PRD.
func budgetHardCapExceeded(ctx context.Context, database *db.DB, processorName string) (bool, error) {
	var exceeded bool
	err := dbtx.QueryRow(ctx, database, `
		SELECT EXISTS (
		  SELECT 1 FROM budgets b
		  WHERE b.enforcement='hard_cap'
		    AND (b.scope='org' OR (b.scope='processor' AND b.scope_id=$1))
		    AND COALESCE((
		      SELECT sum(cost_usd) FROM usage_rollup_hourly u
		      WHERE u.org_id=b.org_id AND u.hour >= date_trunc('month', now())
		        AND ((b.scope='org' AND u.dimension='total')
		          OR (b.scope='processor' AND u.dimension='processor' AND u.dimension_value=b.scope_id))
		    ),0) >= b.limit_usd
		)
	`, processorName).Scan(&exceeded)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return exceeded, err
}
