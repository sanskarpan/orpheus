// Package handlers — tenant S3 delivery destinations (PRD 06).
package handlers

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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
	"github.com/orpheus/api/internal/delivery"
)

// DestinationHandler serves the delivery-destination endpoints.
type DestinationHandler struct {
	DB        *db.DB
	Audit     *audit.Recorder
	Deliverer *delivery.Deliverer
}

// CreateDestinationRequest is the body for POST /v1/destinations.
type CreateDestinationRequest struct {
	Type       string `json:"type"`
	Bucket     string `json:"bucket"`
	Prefix     string `json:"prefix"`
	Region     string `json:"region"`
	RoleARN    string `json:"role_arn"`
	ExternalID string `json:"external_id"`
	Endpoint   string `json:"endpoint"`
}

// DestinationView is the wire shape of a destination (never leaks secrets;
// there are none — STS uses role_arn + external_id, static uses platform creds).
type DestinationView struct {
	ID         string     `json:"id"`
	Type       string     `json:"type"`
	Bucket     string     `json:"bucket"`
	Prefix     string     `json:"prefix,omitempty"`
	Region     string     `json:"region"`
	RoleARN    string     `json:"role_arn,omitempty"`
	ExternalID string     `json:"external_id,omitempty"`
	Endpoint   string     `json:"endpoint,omitempty"`
	VerifiedAt *time.Time `json:"verified_at,omitempty"`
	LastError  string     `json:"last_error,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
}

// Create handles POST /v1/destinations.
func (h *DestinationHandler) Create(w http.ResponseWriter, r *http.Request) {
	p, err := auth.PrincipalFromContext(r.Context())
	if err != nil {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "Unauthorized")
		return
	}
	var req CreateDestinationRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, "validation", "Invalid JSON")
		return
	}
	if req.Type == "" {
		req.Type = "s3_sts"
	}
	if req.Type != "s3_sts" && req.Type != "s3_static" {
		writeProblem(w, http.StatusBadRequest, "validation", "type must be s3_sts or s3_static")
		return
	}
	if req.Bucket == "" {
		writeProblem(w, http.StatusBadRequest, "validation", "bucket required")
		return
	}
	if req.Type == "s3_sts" && req.RoleARN == "" {
		writeProblem(w, http.StatusBadRequest, "validation", "role_arn required for s3_sts")
		return
	}
	if req.Region == "" {
		req.Region = "us-east-1"
	}
	// Generate an external_id when the client didn't supply one (STS
	// confused-deputy defense — the tenant scopes their role's trust to it).
	if req.ExternalID == "" {
		buf := make([]byte, 16)
		_, _ = rand.Read(buf)
		req.ExternalID = "orpheus-" + hex.EncodeToString(buf)
	}

	id := uuid.NewString()
	now := time.Now()
	err = h.DB.WithTenant(r.Context(), p.OrgID, func(ctx context.Context) error {
		_, e := dbtx.Exec(ctx, h.DB, `
			INSERT INTO delivery_destinations (id, org_id, type, bucket, prefix, region, role_arn, external_id, endpoint, created_at)
			VALUES ($1,$2,$3,$4,$5,$6,NULLIF($7,''),$8,NULLIF($9,''),$10)
		`, id, p.OrgID, req.Type, req.Bucket, req.Prefix, req.Region, req.RoleARN, req.ExternalID, req.Endpoint, now)
		return e
	})
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "internal", "Failed to create destination")
		return
	}
	if h.Audit != nil {
		_ = h.Audit.Record(r.Context(), audit.Entry{Action: "billing.plan_change", ResourceType: "destination", ResourceID: id,
			Metadata: map[string]any{"type": req.Type, "bucket": req.Bucket}})
	}
	writeJSON(w, http.StatusCreated, DestinationView{
		ID: id, Type: req.Type, Bucket: req.Bucket, Prefix: req.Prefix, Region: req.Region,
		RoleARN: req.RoleARN, ExternalID: req.ExternalID, Endpoint: req.Endpoint, CreatedAt: now,
	})
}

// List handles GET /v1/destinations.
func (h *DestinationHandler) List(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFromContext(r.Context())
	var out []DestinationView
	err := h.DB.WithTenant(r.Context(), p.OrgID, func(ctx context.Context) error {
		rows, e := dbtx.Query(ctx, h.DB, `
			SELECT id, type, bucket, COALESCE(prefix,''), region, COALESCE(role_arn,''), COALESCE(external_id,''),
			       COALESCE(endpoint,''), verified_at, COALESCE(last_error,''), created_at
			FROM delivery_destinations WHERE org_id=$1 ORDER BY created_at DESC
		`, p.OrgID)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var d DestinationView
			if e := rows.Scan(&d.ID, &d.Type, &d.Bucket, &d.Prefix, &d.Region, &d.RoleARN, &d.ExternalID, &d.Endpoint, &d.VerifiedAt, &d.LastError, &d.CreatedAt); e != nil {
				return e
			}
			out = append(out, d)
		}
		return rows.Err()
	})
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "internal", "Failed to list destinations")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

// Verify handles POST /v1/destinations/{id}/verify — assume the role (or use
// static creds) and write+delete a probe object, recording the outcome.
func (h *DestinationHandler) Verify(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFromContext(r.Context())
	id, ok := uuidParam(r, "id")
	if !ok {
		writeProblem(w, http.StatusNotFound, "not_found", "Destination not found")
		return
	}
	if h.Deliverer == nil {
		writeProblem(w, http.StatusServiceUnavailable, "unconfigured", "Delivery not configured")
		return
	}
	var dest delivery.Destination
	err := h.DB.WithTenant(r.Context(), p.OrgID, func(ctx context.Context) error {
		return dbtx.QueryRow(ctx, h.DB, `
			SELECT type, bucket, COALESCE(prefix,''), region, COALESCE(role_arn,''), COALESCE(external_id,''), COALESCE(endpoint,'')
			FROM delivery_destinations WHERE id=$1
		`, id).Scan(&dest.Type, &dest.Bucket, &dest.Prefix, &dest.Region, &dest.RoleARN, &dest.ExternalID, &dest.Endpoint)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeProblem(w, http.StatusNotFound, "not_found", "Destination not found")
			return
		}
		writeProblem(w, http.StatusInternalServerError, "internal", "Failed to load destination")
		return
	}

	verr := h.Deliverer.Verify(r.Context(), dest)
	_ = h.DB.WithTenant(r.Context(), p.OrgID, func(ctx context.Context) error {
		if verr != nil {
			_, e := dbtx.Exec(ctx, h.DB, `UPDATE delivery_destinations SET last_error=$1, verified_at=NULL WHERE id=$2`, verr.Error(), id)
			return e
		}
		_, e := dbtx.Exec(ctx, h.DB, `UPDATE delivery_destinations SET verified_at=now(), last_error=NULL WHERE id=$1`, id)
		return e
	})
	if h.Audit != nil {
		_ = h.Audit.Record(r.Context(), audit.Entry{Action: "billing.plan_change", ResourceType: "destination", ResourceID: id,
			Metadata: map[string]any{"verify": verr == nil}})
	}
	if verr != nil {
		writeJSON(w, http.StatusOK, map[string]any{"verified": false, "error": verr.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"verified": true})
}

// Delete handles DELETE /v1/destinations/{id}.
func (h *DestinationHandler) Delete(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFromContext(r.Context())
	id, ok := uuidParam(r, "id")
	if !ok {
		writeProblem(w, http.StatusNotFound, "not_found", "Destination not found")
		return
	}
	var n int64
	err := h.DB.WithTenant(r.Context(), p.OrgID, func(ctx context.Context) error {
		tag, e := dbtx.Exec(ctx, h.DB, `DELETE FROM delivery_destinations WHERE id=$1`, id)
		if e != nil {
			return e
		}
		n = tag.RowsAffected()
		return nil
	})
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "internal", "Failed to delete destination")
		return
	}
	if n == 0 {
		writeProblem(w, http.StatusNotFound, "not_found", "Destination not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
