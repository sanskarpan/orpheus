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
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/orpheus/api/internal/audit"
	"github.com/orpheus/api/internal/auth"
	"github.com/orpheus/api/internal/db"
	"github.com/orpheus/api/internal/dbtx"
	"github.com/orpheus/api/internal/metrics"
	"github.com/orpheus/api/internal/outbox"
)

// JobHandler bundles the dependencies the job endpoints need. All
// fields are required; zero values will fail at request time.
type JobHandler struct {
	DB      *db.DB
	Audit   *audit.Recorder
	Metrics *metrics.Metrics
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
	// Cache controls the content-addressed result cache (PRD 01):
	// auto (default) = return a prior result on hit; bypass = force
	// recompute but still populate; only = 409 if not already cached.
	Cache string `json:"cache,omitempty"`
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
	// Cache is "hit" when this job's result was served from the cache,
	// "miss" when it was newly enqueued (omitted when caching didn't apply).
	Cache           string     `json:"cache,omitempty"`
	CachedFromJobID string     `json:"cached_from_job_id,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
	StartedAt       *time.Time `json:"started_at,omitempty"`
	CompletedAt     *time.Time `json:"completed_at,omitempty"`
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

	cacheMode := req.Cache
	if cacheMode == "" {
		cacheMode = "auto"
	}
	if cacheMode != "auto" && cacheMode != "bypass" && cacheMode != "only" {
		writeProblem(w, http.StatusBadRequest, "validation", "cache must be auto, bypass, or only")
		return
	}

	// Look up the processor + version in the public catalog.
	var processorID, versionID, modelVersionID string
	var cacheable bool
	err = h.DB.QueryRow(r.Context(), `
		SELECT p.id::text, pv.id::text, pv.model_version_id, pv.cacheable
		FROM processors p
		JOIN processor_versions pv ON pv.processor_id = p.id
		WHERE p.name = $1 AND pv.version = $2 AND pv.deprecated_at IS NULL
	`, req.Processor.Name, req.Processor.Version).Scan(&processorID, &versionID, &modelVersionID, &cacheable)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeProblem(w, http.StatusBadRequest, "validation", "Unknown or deprecated processor/version")
			return
		}
		writeProblem(w, http.StatusInternalServerError, "internal", "Failed to lookup processor")
		return
	}

	// Verify the artifact belongs to this org and get its content hash.
	// RLS scopes the read to the caller's org automatically.
	var inputHash string
	err = h.DB.WithTenant(r.Context(), p.OrgID, func(ctx context.Context) error {
		return dbtx.QueryRow(ctx, h.DB, `SELECT sha256 FROM artifacts WHERE id = $1`, req.ArtifactID).Scan(&inputHash)
	})
	if err != nil {
		writeProblem(w, http.StatusNotFound, "not_found", "Artifact not found")
		return
	}

	// Content-addressed cache (PRD 01). Compute the key when the processor
	// is cacheable; read it unless the caller asked to bypass.
	var cacheMetaArg any // nil unless we want the worker to populate the cache
	if cacheable {
		paramsHash, herr := canonicalParamsHash(req.Params)
		if herr != nil {
			writeProblem(w, http.StatusBadRequest, "validation", "Invalid params JSON")
			return
		}
		cacheKey := computeCacheKey(inputHash, paramsHash, modelVersionID)

		if cacheMode != "bypass" {
			hit, err := h.serveCacheHit(w, r, p.OrgID, req, cacheKey)
			if err != nil {
				writeProblem(w, http.StatusInternalServerError, "internal", "Cache lookup failed")
				return
			}
			if hit {
				return // response already written
			}
			if cacheMode == "only" {
				writeProblem(w, http.StatusConflict, "cache-miss", "No cached result for this input")
				return
			}
		}
		// auto-miss or bypass → populate the cache when the worker finishes.
		meta, _ := json.Marshal(map[string]string{
			"ck": hex.EncodeToString(cacheKey),
			"ih": inputHash,
			"ph": paramsHash,
			"mv": modelVersionID,
		})
		cacheMetaArg = meta
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
		_, err := dbtx.Exec(ctx, h.DB, `
			INSERT INTO jobs (
				id, org_id, user_id, artifact_id, job_type, params,
				status, priority, max_retries, attempts, version, created_at, updated_at,
				cache_meta
			)
			VALUES (
				$1, $2, NULLIF($3, '')::uuid, $4, 'custom'::job_type, $5::jsonb,
				'queued'::job_status, $6, 3, 0, 1, $7, $7,
				$8::jsonb
			)
		`, id, p.OrgID, p.UserID, req.ArtifactID, paramsArg, req.Priority, now, cacheMetaArg)
		if err != nil {
			return err
		}
		return outbox.Enqueue(ctx, h.DB, outbox.Event{
			OrgID:         p.OrgID,
			AggregateType: "job",
			AggregateID:   id,
			EventType:     "job.queued",
			Payload: map[string]any{
				"job_id":   id,
				"job_type": req.Processor.Name,
			},
		})
	})
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "internal", "Failed to create job")
		return
	}

	if h.Metrics != nil {
		h.Metrics.JobsSubmitted.WithLabelValues(req.Processor.Name).Inc()
	}

	_ = h.Audit.Record(r.Context(), audit.Entry{
		Action:       "job.create",
		ResourceType: "job",
		ResourceID:   id,
		Metadata:     map[string]any{"processor": req.Processor.Name, "version": req.Processor.Version},
	})

	cacheField := ""
	if cacheable {
		cacheField = "miss"
	}
	w.Header().Set("Location", "/v1/jobs/"+id)
	writeJSON(w, http.StatusAccepted, Job{
		ID:          id,
		ArtifactID:  req.ArtifactID,
		Processor:   req.Processor,
		Status:      "queued",
		Params:      req.Params,
		MaxAttempts: 3,
		Cache:       cacheField,
		CreatedAt:   now,
		UpdatedAt:   now,
	})
}

// serveCacheHit looks up a cached result for the given key; on a hit it
// creates a completed job row (cost 0, cache_hit=true), bumps the cache
// entry, emits a job.completed event, writes the 200 response, and returns
// true. On a miss it returns false with the response untouched.
func (h *JobHandler) serveCacheHit(w http.ResponseWriter, r *http.Request, orgID string, req CreateJobRequest, cacheKey []byte) (bool, error) {
	id := uuid.NewString()
	now := time.Now()
	storedParams := mergeProcessorIntoParams(req.Params, req.Processor)
	paramsArg := []byte(`{}`)
	if len(storedParams) > 0 {
		paramsArg = storedParams
	}

	var (
		cachedResult json.RawMessage
		sourceJobID  string
		found        bool
	)
	err := h.DB.WithTenant(r.Context(), orgID, func(ctx context.Context) error {
		res, src, ok, lerr := cacheLookup(ctx, h.DB, cacheKey)
		if lerr != nil || !ok {
			found = ok
			return lerr
		}
		found, cachedResult, sourceJobID = true, res, src

		if _, e := dbtx.Exec(ctx, h.DB, `
			INSERT INTO jobs (
				id, org_id, user_id, artifact_id, job_type, params,
				status, priority, max_retries, attempts, version,
				cache_hit, cached_from_job_id, cost_usd,
				created_at, updated_at, started_at, completed_at
			)
			VALUES (
				$1, $2, NULLIF($3, '')::uuid, $4, 'custom'::job_type, $5::jsonb,
				'completed'::job_status, $6, 3, 0, 1,
				true, $7::uuid, 0,
				$8, $8, $8, $8
			)
		`, id, orgID, "", req.ArtifactID, paramsArg, req.Priority, sourceJobID, now); e != nil {
			return e
		}
		if _, e := dbtx.Exec(ctx, h.DB, `
			UPDATE job_result_cache SET hit_count = hit_count + 1, last_hit_at = now()
			WHERE cache_key = $1
		`, cacheKey); e != nil {
			return e
		}
		return outbox.Enqueue(ctx, h.DB, outbox.Event{
			OrgID:         orgID,
			AggregateType: "job",
			AggregateID:   id,
			EventType:     "job.completed",
			Payload: map[string]any{
				"job_id":             id,
				"job_type":           req.Processor.Name,
				"cache":              "hit",
				"cost_usd":           0,
				"cached_from_job_id": sourceJobID,
			},
		})
	})
	if err != nil || !found {
		return false, err
	}

	if h.Metrics != nil {
		h.Metrics.JobsSubmitted.WithLabelValues(req.Processor.Name).Inc()
	}
	_ = h.Audit.Record(r.Context(), audit.Entry{
		Action:       "job.create",
		ResourceType: "job",
		ResourceID:   id,
		Metadata:     map[string]any{"processor": req.Processor.Name, "cache": "hit", "cached_from_job_id": sourceJobID},
	})

	w.Header().Set("Location", "/v1/jobs/"+id)
	writeJSON(w, http.StatusOK, Job{
		ID:              id,
		ArtifactID:      req.ArtifactID,
		Processor:       req.Processor,
		Status:          "completed",
		Params:          req.Params,
		Result:          cachedResult,
		MaxAttempts:     3,
		Cache:           "hit",
		CachedFromJobID: sourceJobID,
		CostUSD:         0,
		CreatedAt:       now,
		UpdatedAt:       now,
		StartedAt:       &now,
		CompletedAt:     &now,
	})
	return true, nil
}

// Get handles GET /v1/jobs/{id}. RLS scopes the read to the caller's
// org; rows from other orgs (or unknown ids) come back as ErrNoRows.
func (h *JobHandler) Get(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFromContext(r.Context())
	id, ok := uuidParam(r, "id")
	if !ok {
		writeProblem(w, http.StatusNotFound, "not_found", "Job not found")
		return
	}

	var (
		j                      Job
		paramsJSON, resultJSON []byte
		startedAt, completedAt *time.Time
		cacheHit               bool
		cachedFromJobID        *string
	)
	err := h.DB.WithTenant(r.Context(), p.OrgID, func(ctx context.Context) error {
		return dbtx.QueryRow(ctx, h.DB, `
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
				completed_at,
				cache_hit,
				cached_from_job_id::text
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
			&cacheHit,
			&cachedFromJobID,
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
	if cacheHit {
		j.Cache = "hit"
	}
	if cachedFromJobID != nil {
		j.CachedFromJobID = *cachedFromJobID
	}

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

	// Validate the typed query params up front. Without this an invalid
	// value is cast in SQL (e.g. $n::job_status, created_at < $n::cursor,
	// artifact_id::uuid) and surfaces as a 500 leaking a DB error string;
	// a bad client input should be a clean 400.
	if status != "" && !validJobStatus(status) {
		writeProblem(w, http.StatusBadRequest, "validation", "invalid status")
		return
	}
	if !validCursor(cursor) {
		writeProblem(w, http.StatusBadRequest, "validation", "invalid cursor")
		return
	}
	if artifactID != "" {
		if _, err := uuid.Parse(artifactID); err != nil {
			writeProblem(w, http.StatusBadRequest, "validation", "invalid artifact_id")
			return
		}
	}

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
		rows, err := dbtx.Query(ctx, h.DB, query, args...)
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
	id, ok := uuidParam(r, "id")
	if !ok {
		writeProblem(w, http.StatusNotFound, "not_found", "Job not found")
		return
	}

	err := h.DB.WithTenant(r.Context(), p.OrgID, func(ctx context.Context) error {
		var cur string
		if err := dbtx.QueryRow(ctx, h.DB, `SELECT status::text FROM jobs WHERE id = $1`, id).Scan(&cur); err != nil {
			return err
		}
		switch cur {
		case "completed", "failed", "canceled":
			return fmt.Errorf("conflict: job already %s", cur)
		}
		_, err := dbtx.Exec(ctx, h.DB, `
			UPDATE jobs
			SET status = 'canceled'::job_status, updated_at = now(), version = version + 1
			WHERE id = $1 AND status IN ('queued', 'running')
		`, id)
		if err != nil {
			return err
		}
		return outbox.Enqueue(ctx, h.DB, outbox.Event{
			OrgID:         p.OrgID,
			AggregateType: "job",
			AggregateID:   id,
			EventType:     "job.canceled",
			Payload:       map[string]any{"job_id": id},
		})
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

// Requeue handles POST /v1/jobs/{id}/requeue. It returns a dead-lettered
// (or failed) job to the queue with a fresh retry budget and re-emits the
// job.queued event so a worker picks it up. Jobs in any other state are
// rejected with 409.
func (h *JobHandler) Requeue(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFromContext(r.Context())
	id, ok := uuidParam(r, "id")
	if !ok {
		writeProblem(w, http.StatusNotFound, "not_found", "Job not found")
		return
	}

	err := h.DB.WithTenant(r.Context(), p.OrgID, func(ctx context.Context) error {
		var cur string
		var procName *string
		if err := dbtx.QueryRow(ctx, h.DB,
			`SELECT status::text, params->'_processor'->>'name' FROM jobs WHERE id = $1`, id,
		).Scan(&cur, &procName); err != nil {
			return err
		}
		if cur != "dead_letter" && cur != "failed" {
			return fmt.Errorf("conflict: job is %s; only dead_letter or failed jobs can be requeued", cur)
		}
		if _, err := dbtx.Exec(ctx, h.DB, `
			UPDATE jobs
			SET status = 'queued'::job_status, attempts = 0, result = NULL,
			    started_at = NULL, completed_at = NULL, updated_at = now(), version = version + 1
			WHERE id = $1
		`, id); err != nil {
			return err
		}
		return outbox.Enqueue(ctx, h.DB, outbox.Event{
			OrgID:         p.OrgID,
			AggregateType: "job",
			AggregateID:   id,
			EventType:     "job.queued",
			Payload:       map[string]any{"job_id": id, "job_type": nullStringVal(procName)},
		})
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeProblem(w, http.StatusNotFound, "not_found", "Job not found")
			return
		}
		if msg := err.Error(); len(msg) >= 9 && msg[:9] == "conflict:" {
			writeProblem(w, http.StatusConflict, "conflict", msg[len("conflict: "):])
			return
		}
		writeProblem(w, http.StatusInternalServerError, "internal", "Failed to requeue")
		return
	}

	_ = h.Audit.Record(r.Context(), audit.Entry{
		Action:       "job.retry",
		ResourceType: "job",
		ResourceID:   id,
	})
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
			return dbtx.QueryRow(ctx, h.DB, `SELECT EXISTS(SELECT 1 FROM artifacts WHERE id = $1)`, j.ArtifactID).Scan(&artifactExists)
		}); err != nil || !artifactExists {
			resp.Rejected = append(resp.Rejected, BulkRejection{Index: i, Reason: "artifact not found"})
			continue
		}

		if err := h.DB.WithTenant(r.Context(), p.OrgID, func(ctx context.Context) error {
			_, err := dbtx.Exec(ctx, h.DB, `
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
