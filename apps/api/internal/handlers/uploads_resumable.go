// Package handlers — resumable multipart + URL ingest (PRD 09).
package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/orpheus/api/internal/audit"
	"github.com/orpheus/api/internal/auth"
	"github.com/orpheus/api/internal/dbtx"
	"github.com/orpheus/api/internal/outbox"
	"github.com/orpheus/api/internal/ssrfguard"
)

// UploadedPartView is one already-uploaded part.
type UploadedPartView struct {
	PartNumber int    `json:"part_no"`
	ETag       string `json:"etag"`
	Size       int64  `json:"size"`
}

// GetParts handles GET /v1/uploads/{id}/parts — which parts landed (from S3)
// and which are missing, so a client can resume without restarting.
func (h *UploadHandler) GetParts(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFromContext(r.Context())
	id, ok := uuidParam(r, "id")
	if !ok {
		writeProblem(w, http.StatusNotFound, "not_found", "Upload not found")
		return
	}
	var s3Key, uploadID string
	var size int64
	err := h.DB.WithTenant(r.Context(), p.OrgID, func(ctx context.Context) error {
		return dbtx.QueryRow(ctx, h.DB, `SELECT COALESCE(s3_key,''), COALESCE(s3_upload_id,''), size_bytes FROM upload_sessions WHERE id=$1`, id).Scan(&s3Key, &uploadID, &size)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeProblem(w, http.StatusNotFound, "not_found", "Upload not found")
			return
		}
		writeProblem(w, http.StatusInternalServerError, "internal", "Failed to load upload")
		return
	}
	if uploadID == "" {
		writeProblem(w, http.StatusConflict, "invalid_state", "Upload has no active multipart (URL ingest or completed)")
		return
	}

	parts, err := h.S3.ListParts(r.Context(), s3Key, uploadID)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "internal", "Failed to list parts")
		return
	}
	have := map[int]bool{}
	uploaded := make([]UploadedPartView, 0, len(parts))
	for _, pt := range parts {
		have[int(pt.PartNumber)] = true
		uploaded = append(uploaded, UploadedPartView{PartNumber: int(pt.PartNumber), ETag: pt.ETag, Size: pt.Size})
	}
	partCount := int((size + int64(defaultPartSize) - 1) / int64(defaultPartSize))
	if partCount < 1 {
		partCount = 1
	}
	missing := []int{}
	for i := 1; i <= partCount; i++ {
		if !have[i] {
			missing = append(missing, i)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"uploaded": uploaded, "missing": missing, "part_size_bytes": defaultPartSize,
	})
}

// RefreshPartsRequest is the body for POST /v1/uploads/{id}/parts:refresh.
type RefreshPartsRequest struct {
	PartNumbers []int `json:"part_numbers"`
}

// RefreshParts handles POST /v1/uploads/{id}/parts:refresh — fresh presigned
// PUT URLs for the requested (missing) parts.
func (h *UploadHandler) RefreshParts(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFromContext(r.Context())
	id, ok := uuidParam(r, "id")
	if !ok {
		writeProblem(w, http.StatusNotFound, "not_found", "Upload not found")
		return
	}
	var req RefreshPartsRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, "validation", "Invalid JSON")
		return
	}
	if len(req.PartNumbers) == 0 || len(req.PartNumbers) > 10000 {
		writeProblem(w, http.StatusBadRequest, "validation", "1..10000 part_numbers required")
		return
	}
	var s3Key, uploadID string
	err := h.DB.WithTenant(r.Context(), p.OrgID, func(ctx context.Context) error {
		return dbtx.QueryRow(ctx, h.DB, `SELECT COALESCE(s3_key,''), COALESCE(s3_upload_id,'') FROM upload_sessions WHERE id=$1 AND status='pending'`, id).Scan(&s3Key, &uploadID)
	})
	if err != nil || uploadID == "" {
		writeProblem(w, http.StatusNotFound, "not_found", "Incomplete upload not found")
		return
	}
	type refreshed struct {
		PartNumber int       `json:"part_no"`
		URL        string    `json:"url"`
		ExpiresAt  time.Time `json:"expires_at"`
	}
	expires := time.Now().Add(15 * time.Minute)
	out := make([]refreshed, 0, len(req.PartNumbers))
	for _, n := range req.PartNumbers {
		if n < 1 {
			continue
		}
		url, e := h.S3.Presigner().PresignUploadPart(r.Context(), h.S3.Bucket(), s3Key, uploadID, int32(n))
		if e != nil {
			writeProblem(w, http.StatusInternalServerError, "internal", "Failed to presign")
			return
		}
		out = append(out, refreshed{PartNumber: n, URL: url, ExpiresAt: expires})
	}
	writeJSON(w, http.StatusOK, map[string]any{"parts": out})
}

// URLIngestRequest is the body for POST /v1/uploads/url.
type URLIngestRequest struct {
	URL            string `json:"url"`
	Filename       string `json:"filename"`
	ContentType    string `json:"content_type"`
	ExpectedSHA256 string `json:"expected_sha256"`
}

// CreateURLIngest handles POST /v1/uploads/url — fetch audio from a URL into a
// normal artifact via the SSRF-safe orpheus.ingest.url worker job.
func (h *UploadHandler) CreateURLIngest(w http.ResponseWriter, r *http.Request) {
	p, err := auth.PrincipalFromContext(r.Context())
	if err != nil {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "Unauthorized")
		return
	}
	var req URLIngestRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, "validation", "Invalid JSON")
		return
	}
	if req.URL == "" {
		writeProblem(w, http.StatusBadRequest, "validation", "url required")
		return
	}
	// SSRF gate at submit (the worker re-validates on fetch + every redirect).
	if reason := ssrfguard.ValidateURLStatic(req.URL); reason != nil {
		writeProblem(w, http.StatusBadRequest, "ssrf-blocked", "url not allowed: "+reason.Error())
		return
	}
	if req.Filename == "" {
		req.Filename = "ingest"
	}
	if req.ContentType == "" {
		req.ContentType = "application/octet-stream"
	}

	sessionID := uuid.NewString()
	jobID := uuid.NewString()
	key := fmt.Sprintf("uploads/%s/%s/%s", p.OrgID, time.Now().UTC().Format("2006/01/02"), uuid.NewString())
	now := time.Now()
	err = h.DB.WithTenant(r.Context(), p.OrgID, func(ctx context.Context) error {
		if _, e := dbtx.Exec(ctx, h.DB, `
			INSERT INTO upload_sessions
			  (id, org_id, user_id, filename, content_type, size_bytes, status, expires_at, created_at,
			   s3_bucket, s3_key, source, source_url, fetch_status)
			VALUES ($1,$2,NULLIF($3,'')::uuid,$4,$5,0,'pending',$6,$7,$8,$9,'url',$10,'fetching')
		`, sessionID, p.OrgID, p.UserID, req.Filename, req.ContentType, now.Add(24*time.Hour), now, h.S3.Bucket(), key, req.URL); e != nil {
			return e
		}
		jobParams, _ := json.Marshal(map[string]any{
			"_processor":        map[string]string{"name": "ingest.url", "version": "1.0.0"},
			"upload_session_id": sessionID,
			"url":               req.URL,
			"s3_bucket":         h.S3.Bucket(),
			"s3_key":            key,
			"content_type":      req.ContentType,
			"expected_sha256":   req.ExpectedSHA256,
		})
		if _, e := dbtx.Exec(ctx, h.DB, `
			INSERT INTO jobs (id, org_id, user_id, artifact_id, job_type, params, status, max_retries, attempts, version, created_at, updated_at)
			VALUES ($1,$2,NULLIF($3,'')::uuid,NULL,'custom'::job_type,$4::jsonb,'queued'::job_status,3,0,1,$5,$5)
		`, jobID, p.OrgID, p.UserID, jobParams, now); e != nil {
			return e
		}
		return outbox.Enqueue(ctx, h.DB, outbox.Event{
			OrgID: p.OrgID, AggregateType: "job", AggregateID: jobID, EventType: "job.queued",
			Payload: map[string]any{"job_id": jobID, "job_type": "ingest.url"},
		})
	})
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "internal", "Failed to start URL ingest")
		return
	}
	if h.Audit != nil {
		_ = h.Audit.Record(r.Context(), audit.Entry{Action: "upload.create", ResourceType: "upload_session", ResourceID: sessionID,
			Metadata: map[string]any{"source": "url"}})
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"upload_id": sessionID, "status": "fetching", "poll_url": "/v1/uploads/" + sessionID,
	})
}
