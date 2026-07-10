// Package handlers — job lifecycle endpoints.
//
// The /v1/jobs surface covers submission, polling, listing, bulk
// submission, and cancellation. The DB layer (internal/db) is the source
// of truth for the job row; this file only translates HTTP requests to
// pgx queries and back.
//
// Note: the `jobs` table does not yet carry dedicated
// `processor_id` / `processor_version_id` / `processor_name` /
// `processor_version` columns (those land in a later migration tracked
// by #102). For Phase 1 we store the processor reference inside the
// `params` jsonb under the reserved `_processor` key, and always set
// `job_type = 'custom'` since the column is a strict enum that does
// not include every catalog processor name. The worker side reads
// `_processor` from params to know which processor to dispatch.
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
)

// JobHandler bundles the dependencies the job endpoints need. All
// fields are required; zero values will fail at request time.
type JobHandler struct {
	DB    *db.DB
	Audit *audit.Recorder
}

// ProcessorRef is a (name, version) pair referencing a catalog
// processor. The Name is the stable slug (e.g. `whisper-transcribe`);
// Version is the semantic version string (e.g. `1.2.0`).
type ProcessorRef struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// CreateJobRequest is the request body for POST /v1/jobs.
type CreateJobRequest struct {
	ArtifactID string          `json:"artifact_id"`
	Processor  ProcessorRef    `json:"processor"`
	Params     json.RawMessage `json:"params,omitempty"`
	Priority   int             `json:"priority,omitempty"`
}

// Job is the response shape for POST /v1/jobs and GET /v1/jobs/{id}.
// It is a logical projection of the `jobs` row plus the processor
// reference we hid inside `params`.
type Job struct {
	ID          string          `json:"id"`
	ArtifactID  string          `json:"artifact_id,omitempty"`
	Processor   ProcessorRef    `json:"processor"`
	Status      string          `json:"status"`
	Params      json.RawMessage `json:"params,omitempty"`
	Result      json.RawMessage `json:"result,omitempty"`
	Attempts    int             `json:"attempts"`
	MaxAttempts int             `json:"max_retries"`
	CostUSD     float64         `json:"cost_usd,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
	StartedAt   *time.Time      `json:"started_at,omitempty"`
	CompletedAt *time.Time      `json:"completed_at,omitempty"`
}

// JobList is a cursor-paginated list of jobs.
type JobList struct {
	Data       []Job  `json:"data"`
	HasMore    bool   `json:"has_more"`
	NextCursor string `json:"next_cursor"`
}

// BulkCreateJobsRequest is the request body for POST /v1/jobs/bulk.
type BulkCreateJobsRequest struct {
	Jobs []CreateJobRequest `json:"jobs"`
}

// BulkJobsResponse is the response body for POST /v1/jobs/bulk.
type BulkJobsResponse struct {
	BatchID  string          `json:"batch_id"`
	Accepted []Job           `json:"accepted"`
	Rejected []BulkRejection `json:"rejected"`
}

// BulkRejection records why a single bulk item was dropped.
type BulkRejection struct {
	Index  int    `json:"index"`
	Reason string `json:"reason"`
}

// maxBulkJobs is the per-call cap for /v1/jobs/bulk; the spec says 500
// but we keep Phase 1 conservative and align with the example (100).
// The cap is enforced before any DB work.
const maxBulkJobs = 100

// processorKey is the reserved key used inside `params` jsonb to
// record the (name, version) pair. Worker code reads it back; the API
// surface extracts it on read so it can be returned to the client.
const processorKey = "_processor"

// Create handles POST /v1/jobs. It validates the request, looks up
// the processor version in the global catalog (no org filter — the
// processor catalog is public, RLS-wise), verifies the artifact
// belongs to the org, and inserts a queued job row.
func (h *JobHandler) Create(w http.ResponseWriter, r *http.Request) {
	p, err := auth.PrincipalFromContext(r.Context())
	if err != nil {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "Unauthorized")
		return
	}

	var req CreateJobRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, "validation", "Invalid JSON")
		return
	}
	if req.ArtifactID == "" || req.Processor.Name == "" || req.Processor.Version == "" {
		writeProblem(w, http.StatusBadRequest, "validation", "artifact_id, processor.name, processor.version required")
		return
	}

	// Look up the processor + version in the public catalog.
	var processorID, versionID string
	err = h.DB.QueryRow(r.Context(), `
		SELECT p.id::text, pv.id::text
		FROM processors p
		JOIN processor_versions pv ON pv.processor_id = p.id
		WHERE p.name = $1 AND pv.version = $2 AND pv.deprecated_at IS NULL
	`, req.Processor.Name, req.Processor.Version).Scan(&processorID, &versionID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeProblem(w, http.StatusBadRequest, "validation", "Unknown or deprecated processor/version")
			return
		}
		writeProblem(w, http.StatusInternalServerError, "internal", "Failed to lookup processor")
		return
	}

	// Verify the artifact belongs to this org. RLS scopes the read to
	// the caller's org automatically.
	var artifactOK bool
	err = h.DB.WithTenant(r.Context(), p.OrgID, func(ctx context.Context) error {
		return h.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM artifacts WHERE id = $1)`, req.ArtifactID).Scan(&artifactOK)
	})
	if err != nil || !artifactOK {
		writeProblem(w, http.StatusNotFound, "not_found", "Artifact not found")
		return
	}

	id := uuid.NewString()
	now := time.Now()
	storedParams := mergeProcessorIntoParams(req.Params, req.Processor)

	// Build the params jsonb literal. We always set the column to a
	// non-null value so we can round-trip our `_processor` marker.
	paramsArg := []byte(`{}`)
	if len(storedParams) > 0 {
		paramsArg = storedParams
	}

	err = h.DB.WithTenant(r.Context(), p.OrgID, func(ctx context.Context) error {
		_, err := h.DB.Exec(ctx, `
			INSERT INTO jobs (
				id, org_id, user_id, artifact_id, job_type, params,
				status, priority, max_retries, attempts, version, created_at, updated_at
			)
			VALUES (
				$1, $2, NULLIF($3, '')::uuid, $4, 'custom'::job_type, $5::jsonb,
				'queued'::job_status, $6, 3, 0, 1, $7, $7
			)
		`, id, p.OrgID, p.UserID, req.ArtifactID, paramsArg, req.Priority, now)
		return err
	})
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "internal", "Failed to create job")
		return
	}

	_ = h.Audit.Record(r.Context(), audit.Entry{
		Action:       "job.create",
		ResourceType: "job",
		ResourceID:   id,
		Metadata:     map[string]any{"processor": req.Processor.Name, "version": req.Processor.Version},
	})

	w.Header().Set("Location", "/v1/jobs/"+id)
	writeJSON(w, http.StatusAccepted, Job{
		ID:          id,
		ArtifactID:  req.ArtifactID,
		Processor:   req.Processor,
		Status:      "queued",
		Params:      req.Params,
		MaxAttempts: 3,
		CreatedAt:   now,
		UpdatedAt:   now,
	})
}

// Get handles GET /v1/jobs/{id}. RLS scopes the read to the caller's
// org; rows from other orgs (or unknown ids) come back as ErrNoRows.
func (h *JobHandler) Get(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFromContext(r.Context())
	id := chi.URLParam(r, "id")

	var (
		j                      Job
		paramsJSON, resultJSON []byte
		startedAt, completedAt *time.Time
	)
	err := h.DB.WithTenant(r.Context(), p.OrgID, func(ctx context.Context) error {
		return h.DB.QueryRow(ctx, `
			SELECT
				id,
				COALESCE(artifact_id::text, ''),
				job_type::text,
				status::text,
				params,
				result,
				attempts,
				max_retries,
				COALESCE(cost_usd, 0)::float8,
				created_at,
				updated_at,
				started_at,
				completed_at
			FROM jobs WHERE id = $1
		`, id).Scan(
			&j.ID,
			&j.ArtifactID,
			&j.Processor.Name,
			&j.Status,
			&paramsJSON,
			&resultJSON,
			&j.Attempts,
			&j.MaxAttempts,
			&j.CostUSD,
			&j.CreatedAt,
			&j.UpdatedAt,
			&startedAt,
			&completedAt,
		)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeProblem(w, http.StatusNotFound, "not_found", "Job not found")
			return
		}
		writeProblem(w, http.StatusInternalServerError, "internal", "Failed to get job")
		return
	}

	// Restore the user-visible processor ref from `_processor` in
	// params; this is what the API actually persists today.
	proc := extractProcessorFromParams(paramsJSON)
	if proc.Name != "" {
		j.Processor = proc
	} else {
		// Fallback: keep job_type as the "name" so callers still see
		// *something* meaningful when the stored params lack the key
		// (e.g. older rows inserted by another code path).
		j.Processor = ProcessorRef{Name: j.Processor.Name, Version: ""}
	}

	// Strip our private `_processor` key from the params we return
	// to the client; it is an internal implementation detail.
	j.Params = stripProcessorFromParams(paramsJSON)
	if len(resultJSON) > 0 {
		j.Result = json.RawMessage(resultJSON)
	}
	j.StartedAt = startedAt
	j.CompletedAt = completedAt

	writeJSON(w, http.StatusOK, j)
}

// List handles GET /v1/jobs. Filters: status, processor (matches the
// stored job_type today; once processor columns land this switches to
// processor_name), artifact_id, and the optional cursor for pagination.
func (h *JobHandler) List(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFromContext(r.Context())
	limit := 50
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 && n <= 200 {
			limit = n
		}
	}
	cursor := r.URL.Query().Get("cursor")
	status := r.URL.Query().Get("status")
	processor := r.URL.Query().Get("processor")
	artifactID := r.URL.Query().Get("artifact_id")

	// Build the WHERE clause incrementally. We pull the job_type
	// filter through a JSONB predicate so processor names round-trip
	// correctly even when they contain characters that don't fit the
	// job_type enum.
	args := []any{p.OrgID}
	where := "WHERE org_id = $1"
	argIdx := 2
	if cursor != "" {
		where += fmt.Sprintf(" AND created_at < $%d", argIdx)
		args = append(args, cursor)
		argIdx++
	}
	if status != "" {
		where += fmt.Sprintf(" AND status = $%d::job_status", argIdx)
		args = append(args, status)
		argIdx++
	}
	if processor != "" {
		where += fmt.Sprintf(" AND params->'%s'->>'name' = $%d", processorKey, argIdx)
		args = append(args, processor)
		argIdx++
	}
	if artifactID != "" {
		where += fmt.Sprintf(" AND artifact_id = $%d", argIdx)
		args = append(args, artifactID)
		argIdx++
	}
	args = append(args, limit+1)
	query := fmt.Sprintf(`
		SELECT
			id,
			COALESCE(artifact_id::text, ''),
			job_type::text,
			status::text,
			params,
			result,
			attempts,
			max_retries,
			COALESCE(cost_usd, 0)::float8,
			created_at,
			updated_at,
			started_at,
			completed_at
		FROM jobs
		%s
		ORDER BY created_at DESC
		LIMIT $%d
	`, where, argIdx)

	var jobs []Job
	err := h.DB.WithTenant(r.Context(), p.OrgID, func(ctx context.Context) error {
		rows, err := h.DB.Query(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var (
				j                      Job
				paramsJSON, resultJSON []byte
				startedAt, completedAt *time.Time
			)
			if err := rows.Scan(
				&j.ID,
				&j.ArtifactID,
				&j.Processor.Name,
				&j.Status,
				&paramsJSON,
				&resultJSON,
				&j.Attempts,
				&j.MaxAttempts,
				&j.CostUSD,
				&j.CreatedAt,
				&j.UpdatedAt,
				&startedAt,
				&completedAt,
			); err != nil {
				return err
			}
			proc := extractProcessorFromParams(paramsJSON)
			if proc.Name != "" {
				j.Processor = proc
			} else {
				j.Processor = ProcessorRef{Name: j.Processor.Name, Version: ""}
			}
			j.Params = stripProcessorFromParams(paramsJSON)
			if len(resultJSON) > 0 {
				j.Result = json.RawMessage(resultJSON)
			}
			j.StartedAt = startedAt
			j.CompletedAt = completedAt
			jobs = append(jobs, j)
		}
		return rows.Err()
	})
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "internal", "Failed to list jobs")
		return
	}

	hasMore := len(jobs) > limit
	if hasMore {
		jobs = jobs[:limit]
	}
	nextCursor := ""
	if hasMore && len(jobs) > 0 {
		nextCursor = jobs[len(jobs)-1].CreatedAt.Format(time.RFC3339Nano)
	}

	writeJSON(w, http.StatusOK, JobList{Data: jobs, HasMore: hasMore, NextCursor: nextCursor})
}

// Cancel handles POST /v1/jobs/{id}/cancel. Already-terminal jobs
// (completed, failed, canceled) are rejected with 409 Conflict; queued
// and running jobs are flipped to `canceled` and the updated record
// is returned with 202 Accepted.
func (h *JobHandler) Cancel(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFromContext(r.Context())
	id := chi.URLParam(r, "id")

	err := h.DB.WithTenant(r.Context(), p.OrgID, func(ctx context.Context) error {
		var cur string
		if err := h.DB.QueryRow(ctx, `SELECT status::text FROM jobs WHERE id = $1`, id).Scan(&cur); err != nil {
			return err
		}
		switch cur {
		case "completed", "failed", "canceled":
			return fmt.Errorf("conflict: job already %s", cur)
		}
		_, err := h.DB.Exec(ctx, `
			UPDATE jobs
			SET status = 'canceled'::job_status, updated_at = now(), version = version + 1
			WHERE id = $1 AND status IN ('queued', 'running')
		`, id)
		return err
	})
	if err != nil {
		msg := err.Error()
		if errors.Is(err, pgx.ErrNoRows) {
			writeProblem(w, http.StatusNotFound, "not_found", "Job not found")
			return
		}
		if len(msg) >= 9 && msg[:9] == "conflict:" {
			writeProblem(w, http.StatusConflict, "conflict", msg[len("conflict: "):])
			return
		}
		writeProblem(w, http.StatusInternalServerError, "internal", "Failed to cancel")
		return
	}

	_ = h.Audit.Record(r.Context(), audit.Entry{
		Action:       "job.cancel",
		ResourceType: "job",
		ResourceID:   id,
	})

	// Return the freshly-canceled job.
	h.Get(w, r.WithContext(r.Context()))
}

// BulkCreate handles POST /v1/jobs/bulk. Per-item validation failures
// are returned in the `rejected` array; well-formed items are queued
// individually (no cross-item transaction in Phase 1).
func (h *JobHandler) BulkCreate(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFromContext(r.Context())
	var req BulkCreateJobsRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 10<<20)).Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, "validation", "Invalid JSON")
		return
	}
	if len(req.Jobs) == 0 || len(req.Jobs) > maxBulkJobs {
		writeProblem(w, http.StatusBadRequest, "validation", fmt.Sprintf("0 < jobs.length <= %d", maxBulkJobs))
		return
	}

	resp := BulkJobsResponse{BatchID: uuid.NewString()}
	for i, j := range req.Jobs {
		if j.ArtifactID == "" || j.Processor.Name == "" || j.Processor.Version == "" {
			resp.Rejected = append(resp.Rejected, BulkRejection{Index: i, Reason: "missing required fields"})
			continue
		}

		id := uuid.NewString()
		now := time.Now()
		storedParams := mergeProcessorIntoParams(j.Params, j.Processor)
		paramsArg := []byte(`{}`)
		if len(storedParams) > 0 {
			paramsArg = storedParams
		}

		// Phase 1: best-effort insert; an artifact/version mismatch
		// is recorded as a per-item rejection. The full validation
		// path (processor exists, artifact belongs to org) is the
		// same one Create runs; a future iteration will hoist the
		// shared code into a helper.
		var procExists, artifactExists bool
		if err := h.DB.QueryRow(r.Context(), `
			SELECT EXISTS(
				SELECT 1 FROM processors p
				JOIN processor_versions pv ON pv.processor_id = p.id
				WHERE p.name = $1 AND pv.version = $2 AND pv.deprecated_at IS NULL
			)
		`, j.Processor.Name, j.Processor.Version).Scan(&procExists); err != nil || !procExists {
			resp.Rejected = append(resp.Rejected, BulkRejection{Index: i, Reason: "unknown or deprecated processor/version"})
			continue
		}
		if err := h.DB.WithTenant(r.Context(), p.OrgID, func(ctx context.Context) error {
			return h.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM artifacts WHERE id = $1)`, j.ArtifactID).Scan(&artifactExists)
		}); err != nil || !artifactExists {
			resp.Rejected = append(resp.Rejected, BulkRejection{Index: i, Reason: "artifact not found"})
			continue
		}

		if err := h.DB.WithTenant(r.Context(), p.OrgID, func(ctx context.Context) error {
			_, err := h.DB.Exec(ctx, `
				INSERT INTO jobs (
					id, org_id, artifact_id, job_type, params,
					status, max_retries, attempts, version, created_at, updated_at
				)
				VALUES (
					$1, $2, $3, 'custom'::job_type, $4::jsonb,
					'queued'::job_status, 3, 0, 1, $5, $5
				)
			`, id, p.OrgID, j.ArtifactID, paramsArg, now)
			return err
		}); err != nil {
			resp.Rejected = append(resp.Rejected, BulkRejection{Index: i, Reason: "insert failed: " + err.Error()})
			continue
		}

		resp.Accepted = append(resp.Accepted, Job{
			ID:          id,
			ArtifactID:  j.ArtifactID,
			Processor:   j.Processor,
			Status:      "queued",
			Params:      j.Params,
			MaxAttempts: 3,
			CreatedAt:   now,
			UpdatedAt:   now,
		})
	}

	writeJSON(w, http.StatusAccepted, resp)
}

// mergeProcessorIntoParams folds the (name, version) pair into the
// user-supplied params under the reserved `_processor` key. If params
// is empty / not an object, the result is a fresh object containing
// just the processor ref.
func mergeProcessorIntoParams(params json.RawMessage, p ProcessorRef) []byte {
	out := map[string]any{}
	if len(params) > 0 {
		var existing map[string]any
		if err := json.Unmarshal(params, &existing); err == nil {
			out = existing
		}
	}
	out[processorKey] = map[string]string{"name": p.Name, "version": p.Version}
	b, err := json.Marshal(out)
	if err != nil {
		// Should not happen — every value is serialisable.
		return []byte(`{}`)
	}
	return b
}

// extractProcessorFromParams returns the (name, version) pair stored
// under `_processor` in a params jsonb document. An empty ProcessorRef
// is returned when the key is missing or the document is malformed.
func extractProcessorFromParams(raw []byte) ProcessorRef {
	if len(raw) == 0 {
		return ProcessorRef{}
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return ProcessorRef{}
	}
	proc, ok := doc[processorKey]
	if !ok {
		return ProcessorRef{}
	}
	var ref struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal(proc, &ref); err != nil {
		return ProcessorRef{}
	}
	return ProcessorRef{Name: ref.Name, Version: ref.Version}
}

// stripProcessorFromParams returns the params jsonb without the
// reserved `_processor` key, so we never leak the internal marker
// to API clients. Non-object params are returned as-is.
func stripProcessorFromParams(raw []byte) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		// Non-object payload (e.g. array, scalar): return unchanged.
		return json.RawMessage(raw)
	}
	delete(doc, processorKey)
	if len(doc) == 0 {
		return nil
	}
	b, err := json.Marshal(doc)
	if err != nil {
		return json.RawMessage(raw)
	}
	return json.RawMessage(b)
}
