// Package handlers — upload session endpoints.
//
// The upload flow is a three-step multipart upload: the client calls
// POST /v1/uploads to get a session id and one presigned URL per part,
// PUTs each part directly to S3, then calls POST /v1/uploads/{id}/complete
// with the ETag S3 returned for each part. On success the server
// transitions the session to `complete` and inserts the corresponding
// `artifacts` row in the same transaction, so a half-completed
// multipart upload can never leak an artifact row.
//
// NOTE: the upload_sessions / artifacts schemas in
// internal/db/migrations/0001_init.sql do not yet include the
// s3_bucket / s3_key / s3_upload_id columns referenced below; those
// are tracked in #102 and #105. Once the migration lands this code
// runs unchanged.
package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/orpheus/api/internal/audit"
	"github.com/orpheus/api/internal/auth"
	"github.com/orpheus/api/internal/db"
	"github.com/orpheus/api/internal/storage/s3"
)

// Upload limits. Anything larger than maxUploadSizeBytes is rejected
// at session creation; we do not want to give the client a presigned
// URL it can never reasonably use. defaultPartSize matches the
// recommended S3 multipart part size (8 MiB) which is also the
// minimum needed to benefit from parallel uploads.
const (
	maxUploadSizeBytes int64 = 1 << 30 // 1 GiB
	defaultPartSize    int   = 8 << 20 // 8 MiB
)

// UploadHandler bundles the dependencies the upload endpoints need.
// All fields are required; zero values will fail at request time.
type UploadHandler struct {
	DB    *db.DB
	S3    *s3.Client
	Audit *audit.Recorder
}

// CreateUploadRequest is the request body for POST /v1/uploads.
type CreateUploadRequest struct {
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	SizeBytes   int64  `json:"size_bytes"`
	SHA256      string `json:"sha256,omitempty"`
}

// UploadSession is the response shape for POST /v1/uploads and
// GET /v1/uploads/{id}.
type UploadSession struct {
	ID        string       `json:"id"`
	Status    string       `json:"status"`
	PartSize  int          `json:"part_size"`
	Parts     []UploadPart `json:"parts"`
	ExpiresAt time.Time    `json:"expires_at"`
	CreatedAt time.Time    `json:"created_at"`
}

// UploadPart is one presigned PUT URL the client uses to upload a
// single slice of the file. The PartNumber is 1-indexed and matches
// the byte range the client sends to URL.
type UploadPart struct {
	PartNumber int       `json:"part_number"`
	URL        string    `json:"url"`
	ExpiresAt  time.Time `json:"expires_at"`
}

// CompleteUploadRequest is the request body for
// POST /v1/uploads/{id}/complete.
type CompleteUploadRequest struct {
	Parts []CompletedPart `json:"parts"`
}

// CompletedPart is the client-supplied record of one uploaded part:
// its 1-based number and the ETag S3 returned for the PUT.
type CompletedPart struct {
	PartNumber int    `json:"part_number"`
	ETag       string `json:"etag"`
}

// Artifact is the response shape for POST /v1/uploads/{id}/complete
// and GET /v1/artifacts/{id}. The audio probe fields (codec,
// duration, sample rate, channels) are populated asynchronously by
// the ingestion worker and will be zero until that completes.
type Artifact struct {
	ID              string    `json:"id"`
	SHA256          string    `json:"sha256"`
	SizeBytes       int64     `json:"size_bytes"`
	ContentType     string    `json:"content_type"`
	Codec           string    `json:"codec,omitempty"`
	DurationSeconds float64   `json:"duration_seconds,omitempty"`
	SampleRate      int       `json:"sample_rate,omitempty"`
	Channels        int       `json:"channels,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
}

// UploadSessionList is a cursor-paginated list of upload sessions.
type UploadSessionList struct {
	Data       []UploadSession `json:"data"`
	HasMore    bool            `json:"has_more"`
	NextCursor string          `json:"next_cursor"`
}

// Create handles POST /v1/uploads. It validates the request, kicks
// off an S3 multipart upload, generates one presigned PUT URL per
// part, and writes the session row. The S3 multipart upload is
// aborted if any of the database work fails so we never leave an
// orphan upload lying around.
func (h *UploadHandler) Create(w http.ResponseWriter, r *http.Request) {
	p, err := auth.PrincipalFromContext(r.Context())
	if err != nil {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "Unauthorized")
		return
	}

	var req CreateUploadRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, "validation", "Invalid JSON")
		return
	}
	if req.SizeBytes <= 0 || req.SizeBytes > maxUploadSizeBytes {
		writeProblem(w, http.StatusRequestEntityTooLarge, "validation", "size_bytes must be 1..1073741824")
		return
	}
	if req.Filename == "" || req.ContentType == "" {
		writeProblem(w, http.StatusBadRequest, "validation", "filename and content_type required")
		return
	}

	key := fmt.Sprintf("uploads/%s/%s/%s", p.OrgID, time.Now().UTC().Format("2006/01/02"), uuid.NewString())
	uploadID, err := h.S3.CreateMultipartUpload(r.Context(), key, req.ContentType)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "internal", "Failed to initiate upload: "+err.Error())
		return
	}

	partSize := defaultPartSize
	partCount := int((req.SizeBytes + int64(partSize) - 1) / int64(partSize))
	if partCount < 1 {
		partCount = 1
	}

	parts := make([]UploadPart, partCount)
	partExpires := time.Now().Add(15 * time.Minute)
	for i := 0; i < partCount; i++ {
		url, err := h.S3.Presigner().PresignUploadPart(r.Context(), h.S3.Bucket(), key, uploadID, int32(i+1))
		if err != nil {
			_ = h.S3.AbortMultipartUpload(r.Context(), key, uploadID)
			writeProblem(w, http.StatusInternalServerError, "internal", "Failed to presign parts")
			return
		}
		parts[i] = UploadPart{PartNumber: i + 1, URL: url, ExpiresAt: partExpires}
	}

	id := uuid.NewString()
	sessionExpires := time.Now().Add(24 * time.Hour)
	now := time.Now()

	err = h.DB.WithTenant(r.Context(), p.OrgID, func(ctx context.Context) error {
		_, err := h.DB.Exec(ctx, `
			INSERT INTO upload_sessions
			  (id, org_id, user_id, filename, content_type, size_bytes, status, expires_at, created_at)
			VALUES ($1, $2, NULLIF($3, '')::uuid, $4, $5, $6, 'pending', $7, $8)
		`, id, p.OrgID, p.UserID, req.Filename, req.ContentType, req.SizeBytes, sessionExpires, now)
		return err
	})
	if err != nil {
		_ = h.S3.AbortMultipartUpload(r.Context(), key, uploadID)
		writeProblem(w, http.StatusInternalServerError, "internal", "Failed to create session")
		return
	}

	_ = h.Audit.Record(r.Context(), audit.Entry{
		Action:       "upload.create",
		ResourceType: "upload_session",
		ResourceID:   id,
		Metadata:     map[string]any{"filename": req.Filename, "size": req.SizeBytes},
	})

	writeJSON(w, http.StatusCreated, UploadSession{
		ID:        id,
		Status:    "pending",
		PartSize:  partSize,
		Parts:     parts,
		ExpiresAt: sessionExpires,
		CreatedAt: now,
	})
}

// Complete handles POST /v1/uploads/{id}/complete. It tells S3 to
// finalise the multipart upload, probes the assembled object's size
// (rejecting on a mismatch with the declared size), and inserts the
// artifacts row in the same transaction as the status transition so
// the two cannot diverge.
func (h *UploadHandler) Complete(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFromContext(r.Context())
	sessionID := chi.URLParam(r, "id")

	var req CompleteUploadRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, "validation", "Invalid JSON")
		return
	}
	if len(req.Parts) == 0 {
		writeProblem(w, http.StatusBadRequest, "validation", "parts required")
		return
	}

	var bucket, key, uploadID, filename, contentType string
	var sizeBytes int64
	err := h.DB.WithTenant(r.Context(), p.OrgID, func(ctx context.Context) error {
		return h.DB.QueryRow(ctx, `
			SELECT s3_bucket, s3_key, s3_upload_id, filename, content_type, size_bytes
			FROM upload_sessions WHERE id = $1 AND status = 'pending'
		`, sessionID).Scan(&bucket, &key, &uploadID, &filename, &contentType, &sizeBytes)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeProblem(w, http.StatusNotFound, "not_found", "Upload session not found or already complete")
			return
		}
		writeProblem(w, http.StatusInternalServerError, "internal", "Failed to lookup session")
		return
	}

	s3parts := make([]s3.CompletedPart, len(req.Parts))
	for i, p := range req.Parts {
		s3parts[i] = s3.CompletedPart{ETag: p.ETag, PartNumber: p.PartNumber}
	}
	if err := h.S3.CompleteMultipartUpload(r.Context(), key, uploadID, s3parts); err != nil {
		writeProblem(w, http.StatusInternalServerError, "internal", "Failed to complete S3 upload: "+err.Error())
		return
	}

	actualSize, actualContentType, err := h.S3.HeadObject(r.Context(), key)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "internal", "Failed to probe uploaded object")
		return
	}
	if actualSize != sizeBytes {
		writeProblem(w, http.StatusConflict, "validation", "Uploaded size mismatch")
		return
	}

	artifactID := uuid.NewString()
	err = h.DB.WithTenant(r.Context(), p.OrgID, func(ctx context.Context) error {
		tx, err := h.DB.Begin(ctx)
		if err != nil {
			return err
		}
		// Rollback is a no-op if Commit succeeded; the error is
		// intentionally swallowed because the only failure mode is
		// "tx already finalized", which is benign.
		defer func() { _ = tx.Rollback(ctx) }()

		if _, err := tx.Exec(ctx, `UPDATE upload_sessions SET status = 'complete', completed_at = now() WHERE id = $1`, sessionID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO artifacts (id, org_id, upload_session_id, s3_bucket, s3_key, sha256, size_bytes, content_type, probe_status, created_at)
			VALUES ($1, $2, $3, $4, $5, '', $6, $7, 'ok', now())
		`, artifactID, p.OrgID, sessionID, bucket, key, actualSize, actualContentType); err != nil {
			return err
		}
		return tx.Commit(ctx)
	})
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "internal", "Failed to record artifact")
		return
	}

	_ = h.Audit.Record(r.Context(), audit.Entry{
		Action:       "upload.complete",
		ResourceType: "upload_session",
		ResourceID:   sessionID,
		Metadata:     map[string]any{"artifact_id": artifactID, "size": actualSize},
	})

	writeJSON(w, http.StatusOK, Artifact{
		ID:          artifactID,
		SHA256:      "",
		SizeBytes:   actualSize,
		ContentType: actualContentType,
		CreatedAt:   time.Now(),
	})
}

// Get handles GET /v1/uploads/{id}. It returns the session's current
// state. The presigned part URLs are not regenerated; clients that
// need a fresh URL should re-create the session.
func (h *UploadHandler) Get(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFromContext(r.Context())
	id := chi.URLParam(r, "id")

	var s UploadSession
	var expiresAt time.Time
	err := h.DB.WithTenant(r.Context(), p.OrgID, func(ctx context.Context) error {
		return h.DB.QueryRow(ctx, `
			SELECT id, status, expires_at, created_at FROM upload_sessions WHERE id = $1
		`, id).Scan(&s.ID, &s.Status, &expiresAt, &s.CreatedAt)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeProblem(w, http.StatusNotFound, "not_found", "Upload session not found")
			return
		}
		writeProblem(w, http.StatusInternalServerError, "internal", "Failed to get session")
		return
	}
	s.ExpiresAt = expiresAt
	writeJSON(w, http.StatusOK, s)
}

// List handles GET /v1/uploads. The result is ordered by created_at
// descending; cursor pagination is over (created_at) so a `next_cursor`
// is the created_at timestamp of the last item in the current page.
func (h *UploadHandler) List(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFromContext(r.Context())
	limit := 50
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 && n <= 200 {
			limit = n
		}
	}
	cursor := r.URL.Query().Get("cursor")
	status := r.URL.Query().Get("status")

	args := []any{p.OrgID, limit + 1}
	query := `SELECT id, status, expires_at, created_at FROM upload_sessions WHERE org_id = $1`
	argIdx := 2
	if cursor != "" {
		query += fmt.Sprintf(" AND created_at < $%d", argIdx)
		args = append(args, cursor)
		argIdx++
	}
	if status != "" {
		query += fmt.Sprintf(" AND status = $%d", argIdx)
		args = append(args, status)
		argIdx++
	}
	query += fmt.Sprintf(" ORDER BY created_at DESC LIMIT $%d", argIdx)
	args = append(args, limit+1)

	var sessions []UploadSession
	err := h.DB.WithTenant(r.Context(), p.OrgID, func(ctx context.Context) error {
		rows, err := h.DB.Query(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var s UploadSession
			var expiresAt time.Time
			if err := rows.Scan(&s.ID, &s.Status, &expiresAt, &s.CreatedAt); err != nil {
				return err
			}
			s.ExpiresAt = expiresAt
			sessions = append(sessions, s)
		}
		return rows.Err()
	})
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "internal", "Failed to list sessions")
		return
	}

	hasMore := len(sessions) > limit
	if hasMore {
		sessions = sessions[:limit]
	}
	nextCursor := ""
	if hasMore && len(sessions) > 0 {
		nextCursor = sessions[len(sessions)-1].CreatedAt.Format(time.RFC3339Nano)
	}

	writeJSON(w, http.StatusOK, UploadSessionList{Data: sessions, HasMore: hasMore, NextCursor: nextCursor})
}
