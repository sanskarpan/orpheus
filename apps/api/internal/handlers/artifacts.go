// Package handlers — artifact endpoints.
//
// An Artifact is the persistent record of a fully-uploaded audio
// object: it carries the S3 location, a SHA-256, the declared
// content type, and the audio-probe results (codec, duration, sample
// rate, channels). The audio-probe fields are populated by an
// asynchronous worker; until that worker runs, the GET response
// returns the zero values for those fields.
//
// The signed-URL endpoint hands out a time-limited presigned GET URL
// the browser can hit directly. The default TTL is 15 minutes, the
// maximum is 1 hour, matching the constants in
// internal/storage/s3/presign.go.
package handlers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/orpheus/api/internal/auth"
	"github.com/orpheus/api/internal/db"
	"github.com/orpheus/api/internal/storage/s3"
)

// ArtifactHandler bundles the dependencies the artifact endpoints
// need.
type ArtifactHandler struct {
	DB *db.DB
	S3 *s3.Client
}

// SignedURL is the response shape for GET /v1/artifacts/{id}/signed-url.
type SignedURL struct {
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expires_at"`
}

// ArtifactList is a cursor-paginated list of artifacts. The cursor is
// the created_at timestamp of the last item in the current page.
type ArtifactList struct {
	Data       []Artifact `json:"data"`
	HasMore    bool       `json:"has_more"`
	NextCursor string     `json:"next_cursor"`
}

// Get handles GET /v1/artifacts/{id}. It looks the row up under the
// caller's org scope (RLS) and returns the artifact metadata.
func (h *ArtifactHandler) Get(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFromContext(r.Context())
	id := chi.URLParam(r, "id")

	var a Artifact
	var s3Bucket, s3Key, sha256, contentType, codec string
	var dur float64
	var sampleRate, channels int
	err := h.DB.WithTenant(r.Context(), p.OrgID, func(ctx context.Context) error {
		return h.DB.QueryRow(ctx, `
			SELECT id, s3_bucket, s3_key, sha256, size_bytes, content_type, codec, duration_seconds, sample_rate, channels, created_at
			FROM artifacts WHERE id = $1
		`, id).Scan(&a.ID, &s3Bucket, &s3Key, &sha256, &a.SizeBytes, &contentType, &codec, &dur, &sampleRate, &channels, &a.CreatedAt)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeProblem(w, http.StatusNotFound, "not_found", "Artifact not found")
			return
		}
		writeProblem(w, http.StatusInternalServerError, "internal", "Failed to get artifact")
		return
	}
	a.SHA256 = sha256
	a.ContentType = contentType
	a.Codec = codec
	a.DurationSeconds = dur
	a.SampleRate = sampleRate
	a.Channels = channels

	writeJSON(w, http.StatusOK, a)
}

// GetSignedURL handles GET /v1/artifacts/{id}/signed-url. It mints
// a presigned GET URL the browser can hit directly. The expires_in
// query parameter is clamped to [60, 3600] seconds; the storage
// package additionally caps it at 1 hour.
func (h *ArtifactHandler) GetSignedURL(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFromContext(r.Context())
	id := chi.URLParam(r, "id")

	ttl := 900
	if e := r.URL.Query().Get("expires_in"); e != "" {
		if n, err := strconv.Atoi(e); err == nil && n >= 60 && n <= 3600 {
			ttl = n
		}
	}

	var s3Bucket, s3Key string
	err := h.DB.WithTenant(r.Context(), p.OrgID, func(ctx context.Context) error {
		return h.DB.QueryRow(ctx, `SELECT s3_bucket, s3_key FROM artifacts WHERE id = $1`, id).Scan(&s3Bucket, &s3Key)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeProblem(w, http.StatusNotFound, "not_found", "Artifact not found")
			return
		}
		writeProblem(w, http.StatusInternalServerError, "internal", "Failed to get artifact")
		return
	}

	url, err := h.S3.Presigner().PresignGetObject(r.Context(), s3Bucket, s3Key, time.Duration(ttl)*time.Second)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "internal", "Failed to presign")
		return
	}

	writeJSON(w, http.StatusOK, SignedURL{URL: url, ExpiresAt: time.Now().Add(time.Duration(ttl) * time.Second)})
}

// List handles GET /v1/artifacts. Results are ordered by created_at
// descending; cursor pagination is over created_at.
func (h *ArtifactHandler) List(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFromContext(r.Context())
	limit := 50
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 && n <= 200 {
			limit = n
		}
	}
	cursor := r.URL.Query().Get("cursor")
	contentType := r.URL.Query().Get("content_type")

	args := []any{p.OrgID, limit + 1}
	query := `SELECT id, s3_bucket, s3_key, sha256, size_bytes, content_type, codec, duration_seconds, sample_rate, channels, created_at
			  FROM artifacts WHERE org_id = $1`
	argIdx := 2
	if cursor != "" {
		query += fmt.Sprintf(" AND created_at < $%d", argIdx)
		args = append(args, cursor)
		argIdx++
	}
	if contentType != "" {
		query += fmt.Sprintf(" AND content_type = $%d", argIdx)
		args = append(args, contentType)
		argIdx++
	}
	query += fmt.Sprintf(" ORDER BY created_at DESC LIMIT $%d", argIdx)
	args = append(args, limit+1)

	var artifacts []Artifact
	err := h.DB.WithTenant(r.Context(), p.OrgID, func(ctx context.Context) error {
		rows, err := h.DB.Query(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var a Artifact
			var s3Bucket, s3Key, sha256, contentType, codec string
			var dur float64
			var sampleRate, channels int
			if err := rows.Scan(&a.ID, &s3Bucket, &s3Key, &sha256, &a.SizeBytes, &contentType, &codec, &dur, &sampleRate, &channels, &a.CreatedAt); err != nil {
				return err
			}
			a.SHA256 = sha256
			a.ContentType = contentType
			a.Codec = codec
			a.DurationSeconds = dur
			a.SampleRate = sampleRate
			a.Channels = channels
			artifacts = append(artifacts, a)
		}
		return rows.Err()
	})
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "internal", "Failed to list artifacts")
		return
	}

	hasMore := len(artifacts) > limit
	if hasMore {
		artifacts = artifacts[:limit]
	}
	nextCursor := ""
	if hasMore && len(artifacts) > 0 {
		nextCursor = artifacts[len(artifacts)-1].CreatedAt.Format(time.RFC3339Nano)
	}

	writeJSON(w, http.StatusOK, ArtifactList{Data: artifacts, HasMore: hasMore, NextCursor: nextCursor})
}
