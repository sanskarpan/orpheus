// Package handlers — GDPR erasure requests (PRD 10).
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

// ErasureHandler serves the erasure-request endpoints.
type ErasureHandler struct {
	DB    *db.DB
	Audit *audit.Recorder
}

// CreateErasureRequest is the body for POST /v1/erasure-requests.
type CreateErasureRequest struct {
	Scope      string `json:"scope"`
	ArtifactID string `json:"artifact_id"`
	JobID      string `json:"job_id"`
	SubjectRef string `json:"subject_ref"`
	Reason     string `json:"reason"`
	Confirm    bool   `json:"confirm"`
}

// ErasureView is the wire shape of an erasure request.
type ErasureView struct {
	ID              string         `json:"id"`
	Scope           string         `json:"scope"`
	Status          string         `json:"status"`
	Reason          string         `json:"reason,omitempty"`
	DeletedCounts   map[string]int `json:"deleted_counts,omitempty"`
	S3ObjectsPurged int            `json:"s3_objects_purged"`
	CertificateURL  string         `json:"certificate_url,omitempty"`
	PollURL         string         `json:"poll_url,omitempty"`
	Error           string         `json:"error,omitempty"`
	CreatedAt       time.Time      `json:"created_at"`
	CompletedAt     *time.Time     `json:"completed_at,omitempty"`
}

var errLegalHold = errors.New("legal hold")

// Create handles POST /v1/erasure-requests. Requires data:erase (route-gated)
// + confirm:true. A legal-hold on the target blocks erasure.
func (h *ErasureHandler) Create(w http.ResponseWriter, r *http.Request) {
	p, err := auth.PrincipalFromContext(r.Context())
	if err != nil {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "Unauthorized")
		return
	}
	var req CreateErasureRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, "validation", "Invalid JSON")
		return
	}
	if !req.Confirm {
		writeProblem(w, http.StatusBadRequest, "validation", "confirm:true required — erasure is irreversible")
		return
	}
	if req.Scope == "" {
		if req.ArtifactID != "" {
			req.Scope = "artifact"
		} else if req.JobID != "" {
			req.Scope = "job"
		}
	}
	if req.Scope != "artifact" && req.Scope != "job" {
		writeProblem(w, http.StatusBadRequest, "validation", "scope must be artifact or job")
		return
	}
	targetID := req.ArtifactID
	if req.Scope == "job" {
		targetID = req.JobID
	}
	if targetID == "" {
		writeProblem(w, http.StatusBadRequest, "validation", "artifact_id or job_id required")
		return
	}

	id := uuid.NewString()
	now := time.Now()
	err = h.DB.WithTenant(r.Context(), p.OrgID, func(ctx context.Context) error {
		// Verify the target exists in the org and check legal hold.
		var held bool
		var exists bool
		if req.Scope == "artifact" {
			if e := dbtx.QueryRow(ctx, h.DB, `SELECT EXISTS(SELECT 1 FROM artifacts WHERE id=$1), COALESCE((SELECT legal_hold FROM artifacts WHERE id=$1), false)`, targetID).Scan(&exists, &held); e != nil {
				return e
			}
		} else {
			if e := dbtx.QueryRow(ctx, h.DB, `
				SELECT EXISTS(SELECT 1 FROM jobs WHERE id=$1),
				       COALESCE((SELECT bool_or(legal_hold) FROM artifacts WHERE id IN (SELECT artifact_id FROM jobs WHERE id=$1)), false)
			`, targetID).Scan(&exists, &held); e != nil {
				return e
			}
		}
		if !exists {
			return pgx.ErrNoRows
		}
		if held {
			return errLegalHold
		}
		_, e := dbtx.Exec(ctx, h.DB, `
			INSERT INTO erasure_requests (id, org_id, scope, target_id, subject_ref, reason, status, requested_by, scheduled_at, created_at)
			VALUES ($1,$2,$3,$4::uuid,NULLIF($5,''),$6,'scheduled',NULLIF($7,'')::uuid,$8,$8)
		`, id, p.OrgID, req.Scope, targetID, req.SubjectRef, req.Reason, p.UserID, now)
		return e
	})
	if err != nil {
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			writeProblem(w, http.StatusNotFound, "not_found", "Target not found")
		case errors.Is(err, errLegalHold):
			writeProblem(w, http.StatusConflict, "legal-hold", "Target is under legal hold")
		default:
			writeProblem(w, http.StatusInternalServerError, "internal", "Failed to create erasure request")
		}
		return
	}
	if h.Audit != nil {
		_ = h.Audit.Record(r.Context(), audit.Entry{Action: "job.delete", ResourceType: "erasure_request", ResourceID: id,
			Metadata: map[string]any{"scope": req.Scope, "target": targetID, "reason": req.Reason}})
	}
	w.Header().Set("Location", "/v1/erasure-requests/"+id)
	writeJSON(w, http.StatusAccepted, ErasureView{ID: id, Scope: req.Scope, Status: "scheduled", Reason: req.Reason,
		PollURL: "/v1/erasure-requests/" + id, CreatedAt: now})
}

// Get handles GET /v1/erasure-requests/{id}.
func (h *ErasureHandler) Get(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFromContext(r.Context())
	id, ok := uuidParam(r, "id")
	if !ok {
		writeProblem(w, http.StatusNotFound, "not_found", "Erasure request not found")
		return
	}
	var ev ErasureView
	var countsJSON []byte
	var certKey string
	err := h.DB.WithTenant(r.Context(), p.OrgID, func(ctx context.Context) error {
		return dbtx.QueryRow(ctx, h.DB, `
			SELECT id, scope, status, COALESCE(reason,''), deleted_counts, s3_objects_purged, COALESCE(certificate_s3_key,''), COALESCE(error,''), created_at, completed_at
			FROM erasure_requests WHERE id=$1
		`, id).Scan(&ev.ID, &ev.Scope, &ev.Status, &ev.Reason, &countsJSON, &ev.S3ObjectsPurged, &certKey, &ev.Error, &ev.CreatedAt, &ev.CompletedAt)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeProblem(w, http.StatusNotFound, "not_found", "Erasure request not found")
			return
		}
		writeProblem(w, http.StatusInternalServerError, "internal", "Failed to get erasure request")
		return
	}
	_ = json.Unmarshal(countsJSON, &ev.DeletedCounts)
	if certKey != "" {
		ev.CertificateURL = "/v1/erasure-requests/" + ev.ID + "/certificate"
	}
	writeJSON(w, http.StatusOK, ev)
}

// List handles GET /v1/erasure-requests.
func (h *ErasureHandler) List(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFromContext(r.Context())
	var out []ErasureView
	err := h.DB.WithTenant(r.Context(), p.OrgID, func(ctx context.Context) error {
		rows, e := dbtx.Query(ctx, h.DB, `
			SELECT id, scope, status, COALESCE(reason,''), s3_objects_purged, created_at, completed_at
			FROM erasure_requests WHERE org_id=$1 ORDER BY created_at DESC LIMIT 200
		`, p.OrgID)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var ev ErasureView
			if e := rows.Scan(&ev.ID, &ev.Scope, &ev.Status, &ev.Reason, &ev.S3ObjectsPurged, &ev.CreatedAt, &ev.CompletedAt); e != nil {
				return e
			}
			out = append(out, ev)
		}
		return rows.Err()
	})
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "internal", "Failed to list erasure requests")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}
