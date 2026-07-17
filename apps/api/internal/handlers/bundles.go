// Package handlers — artifact download bundles (PRD 02).
//
// A bundle collects a set of artifacts (and optionally the result JSON of
// source jobs) into a single zip in S3, downloadable via one signed,
// expiring URL. Creation resolves + RLS-checks every source, records the
// bundle, and enqueues the export.bundle job that does the zipping.
package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/orpheus/api/internal/audit"
	"github.com/orpheus/api/internal/auth"
	"github.com/orpheus/api/internal/db"
	"github.com/orpheus/api/internal/dbtx"
	"github.com/orpheus/api/internal/outbox"
	"github.com/orpheus/api/internal/storage/s3"
)

// bundleTTLDefault / bundleTTLMax bound the download-link lifetime.
const (
	bundleTTLDefault = 3600
	bundleTTLMax     = 24 * 3600
)

// BundleHandler bundles the deps the bundle endpoints need.
type BundleHandler struct {
	DB    *db.DB
	S3    *s3.Client
	Audit *audit.Recorder
}

// BundleSource is one item to include: either an artifact or a job's result.
type BundleSource struct {
	JobID      string `json:"job_id,omitempty"`
	ArtifactID string `json:"artifact_id,omitempty"`
}

// CreateBundleRequest is the body for POST /v1/bundles.
type CreateBundleRequest struct {
	Name              string         `json:"name"`
	Sources           []BundleSource `json:"sources"`
	IncludeResultJSON *bool          `json:"include_result_json,omitempty"`
	TTLSeconds        int            `json:"ttl_seconds,omitempty"`
}

// BundleView is the wire shape of a bundle row.
type BundleView struct {
	ID            string     `json:"id"`
	Name          string     `json:"name,omitempty"`
	Status        string     `json:"status"`
	SizeBytes     int64      `json:"size_bytes"`
	ArtifactCount int        `json:"artifact_count"`
	Error         string     `json:"error,omitempty"`
	PollURL       string     `json:"poll_url,omitempty"`
	ExpiresAt     *time.Time `json:"expires_at,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
}

// Create handles POST /v1/bundles. Resolves + RLS-checks each source, writes
// the bundle + items, and enqueues the export.bundle job.
func (h *BundleHandler) Create(w http.ResponseWriter, r *http.Request) {
	p, err := auth.PrincipalFromContext(r.Context())
	if err != nil {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "Unauthorized")
		return
	}
	var req CreateBundleRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, "validation", "Invalid JSON")
		return
	}
	if len(req.Sources) == 0 {
		writeProblem(w, http.StatusBadRequest, "validation", "at least one source required")
		return
	}
	if len(req.Sources) > 1000 {
		writeProblem(w, http.StatusBadRequest, "bundle-too-large", "too many sources (max 1000)")
		return
	}
	includeResults := req.IncludeResultJSON == nil || *req.IncludeResultJSON
	ttl := req.TTLSeconds
	if ttl <= 0 {
		ttl = bundleTTLDefault
	}
	if ttl > bundleTTLMax {
		ttl = bundleTTLMax
	}

	bundleID := uuid.NewString()
	jobID := uuid.NewString()
	now := time.Now()
	expiresAt := now.Add(time.Duration(ttl) * time.Second)

	// items: artifact_id -> path_in_zip. resultDocs: path -> job result JSON.
	items := map[string]string{}
	resultDocs := map[string]json.RawMessage{}

	err = h.DB.WithTenant(r.Context(), p.OrgID, func(ctx context.Context) error {
		for _, src := range req.Sources {
			switch {
			case src.ArtifactID != "":
				var key string
				if e := dbtx.QueryRow(ctx, h.DB, `SELECT s3_key FROM artifacts WHERE id = $1`, src.ArtifactID).Scan(&key); e != nil {
					return e // ErrNoRows → 404 (cross-tenant or missing)
				}
				name := path.Base(key)
				if name == "" || name == "." || name == "/" {
					name = "artifact-" + src.ArtifactID
				}
				items[src.ArtifactID] = name
			case src.JobID != "":
				var result []byte
				if e := dbtx.QueryRow(ctx, h.DB, `SELECT COALESCE(result, '{}'::jsonb) FROM jobs WHERE id = $1`, src.JobID).Scan(&result); e != nil {
					return e
				}
				if includeResults {
					resultDocs["job-"+src.JobID+".result.json"] = json.RawMessage(result)
				}
			default:
				return errBadSource
			}
		}
		if len(items) == 0 && len(resultDocs) == 0 {
			return errEmptyBundle
		}

		// Insert the zipping job first — the bundle FKs to it. No single
		// input artifact; the processor reads bundle_items. Processor name
		// lives in params._processor like every other job.
		jobParams, _ := json.Marshal(map[string]any{
			"_processor": map[string]string{"name": "export.bundle", "version": "1.0.0"},
			"bundle_id":  bundleID,
		})
		if _, e := dbtx.Exec(ctx, h.DB, `
			INSERT INTO jobs (id, org_id, user_id, artifact_id, job_type, params, status, priority, max_retries, attempts, version, created_at, updated_at)
			VALUES ($1, $2, NULLIF($3,'')::uuid, NULL, 'custom'::job_type, $4::jsonb, 'queued'::job_status, 0, 3, 0, 1, $5, $5)
		`, jobID, p.OrgID, p.UserID, jobParams, now); e != nil {
			return e
		}

		docsJSON, _ := json.Marshal(resultDocs)
		if _, e := dbtx.Exec(ctx, h.DB, `
			INSERT INTO bundles (id, org_id, name, status, include_result_json, result_docs, job_id, expires_at, created_by, created_at, updated_at)
			VALUES ($1, $2, $3, 'building', $4, $5::jsonb, $6, $7, NULLIF($8,'')::uuid, $9, $9)
		`, bundleID, p.OrgID, req.Name, includeResults, docsJSON, jobID, expiresAt, p.UserID, now); e != nil {
			return e
		}
		for artifactID, pth := range items {
			if _, e := dbtx.Exec(ctx, h.DB, `
				INSERT INTO bundle_items (bundle_id, org_id, artifact_id, path_in_zip) VALUES ($1, $2, $3, $4)
			`, bundleID, p.OrgID, artifactID, pth); e != nil {
				return e
			}
		}

		return outbox.Enqueue(ctx, h.DB, outbox.Event{
			OrgID: p.OrgID, AggregateType: "job", AggregateID: jobID, EventType: "job.queued",
			Payload: map[string]any{"job_id": jobID, "job_type": "export.bundle"},
		})
	})
	if err != nil {
		switch {
		case errors.Is(err, pgx.ErrNoRows), errors.Is(err, errBadSource):
			writeProblem(w, http.StatusNotFound, "not_found", "One or more sources not found")
		case errors.Is(err, errEmptyBundle):
			writeProblem(w, http.StatusBadRequest, "validation", "Bundle would be empty")
		default:
			writeProblem(w, http.StatusInternalServerError, "internal", "Failed to create bundle")
		}
		return
	}

	if h.Audit != nil {
		_ = h.Audit.Record(r.Context(), audit.Entry{
			Action: "bundle.create", ResourceType: "bundle", ResourceID: bundleID,
			Metadata: map[string]any{"artifact_count": len(items), "result_docs": len(resultDocs)},
		})
	}
	w.Header().Set("Location", "/v1/bundles/"+bundleID)
	writeJSON(w, http.StatusAccepted, BundleView{
		ID: bundleID, Name: req.Name, Status: "building", ArtifactCount: len(items) + len(resultDocs),
		PollURL: "/v1/bundles/" + bundleID, ExpiresAt: &expiresAt, CreatedAt: now,
	})
}

var (
	errBadSource   = errors.New("source must set artifact_id or job_id")
	errEmptyBundle = errors.New("empty bundle")
)

// Get handles GET /v1/bundles/{id}.
func (h *BundleHandler) Get(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFromContext(r.Context())
	id, ok := uuidParam(r, "id")
	if !ok {
		writeProblem(w, http.StatusNotFound, "not_found", "Bundle not found")
		return
	}
	bv, err := h.loadBundle(r, p.OrgID, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeProblem(w, http.StatusNotFound, "not_found", "Bundle not found")
			return
		}
		writeProblem(w, http.StatusInternalServerError, "internal", "Failed to get bundle")
		return
	}
	writeJSON(w, http.StatusOK, bv)
}

// Download handles GET /v1/bundles/{id}/download → 302 to a signed S3 URL.
func (h *BundleHandler) Download(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFromContext(r.Context())
	id, ok := uuidParam(r, "id")
	if !ok {
		writeProblem(w, http.StatusNotFound, "not_found", "Bundle not found")
		return
	}
	var (
		status, s3Bucket, s3Key string
		expiresAt               *time.Time
	)
	err := h.DB.WithTenant(r.Context(), p.OrgID, func(ctx context.Context) error {
		return dbtx.QueryRow(ctx, h.DB, `SELECT status, COALESCE(s3_bucket,''), COALESCE(s3_key,''), expires_at FROM bundles WHERE id = $1`, id).
			Scan(&status, &s3Bucket, &s3Key, &expiresAt)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeProblem(w, http.StatusNotFound, "not_found", "Bundle not found")
			return
		}
		writeProblem(w, http.StatusInternalServerError, "internal", "Failed to load bundle")
		return
	}
	if status != "ready" || s3Key == "" {
		writeProblem(w, http.StatusConflict, "not_ready", "Bundle is not ready")
		return
	}
	if expiresAt != nil && time.Now().After(*expiresAt) {
		writeProblem(w, http.StatusGone, "expired", "Bundle has expired")
		return
	}
	ttl := bundleTTLDefault
	if expiresAt != nil {
		if rem := int(time.Until(*expiresAt).Seconds()); rem > 60 && rem < ttl {
			ttl = rem
		}
	}
	url, err := h.S3.Presigner().PresignGetObject(r.Context(), s3Bucket, s3Key, time.Duration(ttl)*time.Second)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "internal", "Failed to presign")
		return
	}
	http.Redirect(w, r, url, http.StatusFound)
}

// Delete handles DELETE /v1/bundles/{id}: revoke — hard-delete the zip and
// mark revoked so a leaked link 404s at S3 and /download 409s.
func (h *BundleHandler) Delete(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFromContext(r.Context())
	id, ok := uuidParam(r, "id")
	if !ok {
		writeProblem(w, http.StatusNotFound, "not_found", "Bundle not found")
		return
	}
	var s3Key string
	err := h.DB.WithTenant(r.Context(), p.OrgID, func(ctx context.Context) error {
		if e := dbtx.QueryRow(ctx, h.DB, `SELECT COALESCE(s3_key,'') FROM bundles WHERE id = $1`, id).Scan(&s3Key); e != nil {
			return e
		}
		_, e := dbtx.Exec(ctx, h.DB, `UPDATE bundles SET status = 'revoked', updated_at = now() WHERE id = $1`, id)
		return e
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeProblem(w, http.StatusNotFound, "not_found", "Bundle not found")
			return
		}
		writeProblem(w, http.StatusInternalServerError, "internal", "Failed to revoke bundle")
		return
	}
	if s3Key != "" && h.S3 != nil {
		// Best-effort: the row is already revoked, so a failed delete only
		// leaves an orphan the S3 lifecycle rule will sweep.
		_ = h.S3.DeleteObject(r.Context(), s3Key)
	}
	if h.Audit != nil {
		_ = h.Audit.Record(r.Context(), audit.Entry{Action: "bundle.delete", ResourceType: "bundle", ResourceID: id})
	}
	w.WriteHeader(http.StatusNoContent)
}

// List handles GET /v1/bundles (org-scoped, newest first).
func (h *BundleHandler) List(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFromContext(r.Context())
	limit := 50
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 && n <= 200 {
			limit = n
		}
	}
	var out []BundleView
	err := h.DB.WithTenant(r.Context(), p.OrgID, func(ctx context.Context) error {
		rows, e := dbtx.Query(ctx, h.DB, `
			SELECT id, name, status, size_bytes, artifact_count, COALESCE(error,''), expires_at, created_at
			FROM bundles WHERE org_id = $1 ORDER BY created_at DESC LIMIT $2
		`, p.OrgID, limit)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var bv BundleView
			if e := rows.Scan(&bv.ID, &bv.Name, &bv.Status, &bv.SizeBytes, &bv.ArtifactCount, &bv.Error, &bv.ExpiresAt, &bv.CreatedAt); e != nil {
				return e
			}
			out = append(out, bv)
		}
		return rows.Err()
	})
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "internal", "Failed to list bundles")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

func (h *BundleHandler) loadBundle(r *http.Request, orgID, id string) (BundleView, error) {
	var bv BundleView
	err := h.DB.WithTenant(r.Context(), orgID, func(ctx context.Context) error {
		return dbtx.QueryRow(ctx, h.DB, `
			SELECT id, name, status, size_bytes, artifact_count, COALESCE(error,''), expires_at, created_at
			FROM bundles WHERE id = $1
		`, id).Scan(&bv.ID, &bv.Name, &bv.Status, &bv.SizeBytes, &bv.ArtifactCount, &bv.Error, &bv.ExpiresAt, &bv.CreatedAt)
	})
	return bv, err
}
