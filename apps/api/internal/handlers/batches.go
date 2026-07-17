// Package handlers — tracked batches (PRD 06).
//
// A batch is a set of jobs submitted as one unit, tracked with aggregate
// counts and finalized by the batching service (internal/batching), which
// fires the batch.completed callback and pushes results to a destination.
package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
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

const maxBatchJobs = 5000

// BatchHandler serves the batch endpoints.
type BatchHandler struct {
	DB    *db.DB
	Audit *audit.Recorder
	S3    *s3.Client
}

// CreateBatchRequest is the body for POST /v1/batches.
type CreateBatchRequest struct {
	Name     string             `json:"name"`
	Jobs     []CreateJobRequest `json:"jobs"`
	Callback *struct {
		WebhookID string `json:"webhook_id"`
	} `json:"callback,omitempty"`
	Delivery *struct {
		DestinationID string `json:"destination_id"`
	} `json:"delivery,omitempty"`
}

// BatchView is the wire shape of a batch.
type BatchView struct {
	ID             string    `json:"id"`
	Name           string    `json:"name,omitempty"`
	Status         string    `json:"status"`
	JobCount       int       `json:"job_count"`
	CompletedCount int       `json:"completed_count"`
	FailedCount    int       `json:"failed_count"`
	ManifestURL    string    `json:"manifest_url,omitempty"`
	PollURL        string    `json:"poll_url,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}

// Create handles POST /v1/batches — atomic: every job must be valid or the
// whole batch is rejected.
func (h *BatchHandler) Create(w http.ResponseWriter, r *http.Request) {
	p, err := auth.PrincipalFromContext(r.Context())
	if err != nil {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "Unauthorized")
		return
	}
	var req CreateBatchRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 20<<20)).Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, "validation", "Invalid JSON")
		return
	}
	if len(req.Jobs) == 0 || len(req.Jobs) > maxBatchJobs {
		writeProblem(w, http.StatusBadRequest, "validation", "1..5000 jobs required")
		return
	}
	for _, j := range req.Jobs {
		if j.ArtifactID == "" || j.Processor.Name == "" || j.Processor.Version == "" {
			writeProblem(w, http.StatusBadRequest, "validation", "each job needs artifact_id + processor")
			return
		}
	}
	var callbackID, destID any
	if req.Callback != nil && req.Callback.WebhookID != "" {
		callbackID = req.Callback.WebhookID
	}
	if req.Delivery != nil && req.Delivery.DestinationID != "" {
		destID = req.Delivery.DestinationID
	}

	batchID := uuid.NewString()
	now := time.Now()
	err = h.DB.WithTenant(r.Context(), p.OrgID, func(ctx context.Context) error {
		// Validate the callback endpoint + destination belong to the org.
		if callbackID != nil {
			var ok bool
			if e := dbtx.QueryRow(ctx, h.DB, `SELECT EXISTS(SELECT 1 FROM webhook_endpoints WHERE id=$1)`, callbackID).Scan(&ok); e != nil || !ok {
				return errBadRef
			}
		}
		if destID != nil {
			var ok bool
			if e := dbtx.QueryRow(ctx, h.DB, `SELECT EXISTS(SELECT 1 FROM delivery_destinations WHERE id=$1)`, destID).Scan(&ok); e != nil || !ok {
				return errBadRef
			}
		}
		// Validate every job before creating anything.
		for _, j := range req.Jobs {
			var procOK bool
			if e := h.DB.QueryRow(r.Context(), `
				SELECT EXISTS(SELECT 1 FROM processors pr JOIN processor_versions pv ON pv.processor_id=pr.id
				  WHERE pr.name=$1 AND pv.version=$2 AND pv.deprecated_at IS NULL)`,
				j.Processor.Name, j.Processor.Version).Scan(&procOK); e != nil || !procOK {
				return errBadRef
			}
			var artOK bool
			if e := dbtx.QueryRow(ctx, h.DB, `SELECT EXISTS(SELECT 1 FROM artifacts WHERE id=$1)`, j.ArtifactID).Scan(&artOK); e != nil || !artOK {
				return errBadRef
			}
		}

		if _, e := dbtx.Exec(ctx, h.DB, `
			INSERT INTO batches (id, org_id, name, status, job_count, callback_webhook_id, destination_id, created_at, updated_at)
			VALUES ($1,$2,$3,'running',$4,$5::uuid,$6::uuid,$7,$7)
		`, batchID, p.OrgID, req.Name, len(req.Jobs), callbackID, destID, now); e != nil {
			return e
		}
		for _, j := range req.Jobs {
			jobID := uuid.NewString()
			storedParams := mergeProcessorIntoParams(j.Params, j.Processor)
			paramsArg := []byte(`{}`)
			if len(storedParams) > 0 {
				paramsArg = storedParams
			}
			if _, e := dbtx.Exec(ctx, h.DB, `
				INSERT INTO jobs (id, org_id, artifact_id, batch_id, job_type, params, status, max_retries, attempts, version, created_at, updated_at)
				VALUES ($1,$2,$3,$4,'custom'::job_type,$5::jsonb,'queued'::job_status,3,0,1,$6,$6)
			`, jobID, p.OrgID, j.ArtifactID, batchID, paramsArg, now); e != nil {
				return e
			}
			if e := outbox.Enqueue(ctx, h.DB, outbox.Event{
				OrgID: p.OrgID, AggregateType: "job", AggregateID: jobID, EventType: "job.queued",
				Payload: map[string]any{"job_id": jobID, "job_type": j.Processor.Name, "batch_id": batchID},
			}); e != nil {
				return e
			}
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, errBadRef) {
			writeProblem(w, http.StatusBadRequest, "validation", "unknown job, callback webhook, or destination")
			return
		}
		writeProblem(w, http.StatusInternalServerError, "internal", "Failed to create batch")
		return
	}
	if h.Audit != nil {
		_ = h.Audit.Record(r.Context(), audit.Entry{Action: "job.create", ResourceType: "batch", ResourceID: batchID,
			Metadata: map[string]any{"job_count": len(req.Jobs)}})
	}
	w.Header().Set("Location", "/v1/batches/"+batchID)
	writeJSON(w, http.StatusAccepted, BatchView{
		ID: batchID, Name: req.Name, Status: "running", JobCount: len(req.Jobs),
		PollURL: "/v1/batches/" + batchID, CreatedAt: now,
	})
}

var errBadRef = errors.New("bad reference")

// Get handles GET /v1/batches/{id}.
func (h *BatchHandler) Get(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFromContext(r.Context())
	id, ok := uuidParam(r, "id")
	if !ok {
		writeProblem(w, http.StatusNotFound, "not_found", "Batch not found")
		return
	}
	var bv BatchView
	var manifestKey string
	err := h.DB.WithTenant(r.Context(), p.OrgID, func(ctx context.Context) error {
		return dbtx.QueryRow(ctx, h.DB, `
			SELECT id, name, status, job_count, completed_count, failed_count, COALESCE(manifest_s3_key,''), created_at
			FROM batches WHERE id=$1
		`, id).Scan(&bv.ID, &bv.Name, &bv.Status, &bv.JobCount, &bv.CompletedCount, &bv.FailedCount, &manifestKey, &bv.CreatedAt)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeProblem(w, http.StatusNotFound, "not_found", "Batch not found")
			return
		}
		writeProblem(w, http.StatusInternalServerError, "internal", "Failed to get batch")
		return
	}
	if manifestKey != "" {
		bv.ManifestURL = "/v1/batches/" + bv.ID + "/manifest"
	}
	writeJSON(w, http.StatusOK, bv)
}

// Manifest handles GET /v1/batches/{id}/manifest → 302 to a signed URL.
func (h *BatchHandler) Manifest(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFromContext(r.Context())
	id, ok := uuidParam(r, "id")
	if !ok {
		writeProblem(w, http.StatusNotFound, "not_found", "Batch not found")
		return
	}
	var key string
	err := h.DB.WithTenant(r.Context(), p.OrgID, func(ctx context.Context) error {
		return dbtx.QueryRow(ctx, h.DB, `SELECT COALESCE(manifest_s3_key,'') FROM batches WHERE id=$1`, id).Scan(&key)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeProblem(w, http.StatusNotFound, "not_found", "Batch not found")
			return
		}
		writeProblem(w, http.StatusInternalServerError, "internal", "Failed to load batch")
		return
	}
	if key == "" || h.S3 == nil {
		writeProblem(w, http.StatusConflict, "not_ready", "Manifest not available")
		return
	}
	url, err := h.S3.Presigner().PresignGetObject(r.Context(), h.S3.Bucket(), key, 15*time.Minute)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "internal", "Failed to presign")
		return
	}
	http.Redirect(w, r, url, http.StatusFound)
}

// ListJobs handles GET /v1/batches/{id}/jobs — the batch's child jobs.
func (h *BatchHandler) ListJobs(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFromContext(r.Context())
	id, ok := uuidParam(r, "id")
	if !ok {
		writeProblem(w, http.StatusNotFound, "not_found", "Batch not found")
		return
	}
	limit := 100
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, e := strconv.Atoi(l); e == nil && n > 0 && n <= 500 {
			limit = n
		}
	}
	type childJob struct {
		ID             string `json:"id"`
		Status         string `json:"status"`
		DeliveryStatus string `json:"delivery_status,omitempty"`
	}
	var jobs []childJob
	err := h.DB.WithTenant(r.Context(), p.OrgID, func(ctx context.Context) error {
		rows, e := dbtx.Query(ctx, h.DB, `
			SELECT id::text, status::text, COALESCE(delivery_status,'')
			FROM jobs WHERE batch_id=$1 ORDER BY created_at LIMIT $2
		`, id, limit)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var c childJob
			if e := rows.Scan(&c.ID, &c.Status, &c.DeliveryStatus); e != nil {
				return e
			}
			jobs = append(jobs, c)
		}
		return rows.Err()
	})
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "internal", "Failed to list batch jobs")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": jobs})
}
