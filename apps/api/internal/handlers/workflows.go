// Package handlers — workflow endpoints.
//
// A workflow is a higher-level user-facing request that may involve
// multiple jobs. The simplest workflow (transcribe-long) creates
// one transcribe job and tracks its result in a workflows row.
package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/orpheus/api/internal/audit"
	"github.com/orpheus/api/internal/auth"
	"github.com/orpheus/api/internal/db"
	"github.com/orpheus/api/internal/dbtx"
	"github.com/orpheus/api/internal/outbox"
)

type WorkflowHandler struct {
	DB    *db.DB
	Audit *audit.Recorder
}

type CreateTranscribeLongRequest struct {
	ArtifactID string          `json:"artifact_id"`
	Params     json.RawMessage `json:"params,omitempty"`
}

type Workflow struct {
	ID           string          `json:"id"`
	Type         string          `json:"type"`
	Status       string          `json:"status"`
	ArtifactID   string          `json:"artifact_id"`
	Params       json.RawMessage `json:"params,omitempty"`
	Result       json.RawMessage `json:"result,omitempty"`
	CurrentJobID *string         `json:"current_job_id,omitempty"`
	Error        string          `json:"error,omitempty"`
	CreatedAt    time.Time       `json:"created_at"`
	UpdatedAt    time.Time       `json:"updated_at"`
}

func (h *WorkflowHandler) CreateTranscribeLong(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFromContext(r.Context())
	var req CreateTranscribeLongRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, "validation", "Invalid JSON")
		return
	}
	if req.ArtifactID == "" {
		writeProblem(w, http.StatusBadRequest, "validation", "artifact_id required")
		return
	}
	artifactUUID, err := uuid.Parse(req.ArtifactID)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "validation", "artifact_id must be a UUID")
		return
	}
	var paramsArg any = []byte("{}")
	if len(req.Params) > 0 {
		paramsArg = []byte(req.Params)
	}

	var workflowID, jobID string
	err = h.DB.WithTenant(r.Context(), p.OrgID, func(ctx context.Context) error {
		tx, err := h.DB.Begin(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(ctx) }()

		txCtx := dbtx.WithTx(ctx, tx)

		if err := tx.QueryRow(txCtx, `
			INSERT INTO workflows (org_id, type, status, artifact_id, params)
			VALUES ($1, 'transcribe-long', 'queued', $2, $3)
			RETURNING id::text
		`, p.OrgID, artifactUUID, paramsArg).Scan(&workflowID); err != nil {
			return err
		}

		merged := mergeWorkflowID(req.Params, workflowID)
		jobID = uuid.NewString()
		if _, err := tx.Exec(txCtx, `
			INSERT INTO jobs (
				id, org_id, user_id, artifact_id, job_type, params,
				status, priority, max_retries, attempts, version, created_at, updated_at
			)
			VALUES (
				$1, $2, NULLIF($3, '')::uuid, $4, 'custom'::job_type, $5::jsonb,
				'queued'::job_status, 0, 3, 0, 1, now(), now()
			)
		`, jobID, p.OrgID, p.UserID, artifactUUID, merged); err != nil {
			return err
		}

		if _, err := tx.Exec(txCtx, `UPDATE workflows SET current_job_id = $1 WHERE id = $2`, jobID, workflowID); err != nil {
			return err
		}

		if err := outbox.Enqueue(txCtx, h.DB, outbox.Event{
			OrgID:         p.OrgID,
			AggregateType: "job",
			AggregateID:   jobID,
			EventType:     "job.queued",
			Payload: map[string]any{
				"job_id":   jobID,
				"job_type": "transcribe",
			},
		}); err != nil {
			return err
		}
		return tx.Commit(txCtx)
	})
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "internal", "Failed to create workflow")
		return
	}

	_ = h.Audit.Record(r.Context(), audit.Entry{
		Action:       "workflow.create",
		ResourceType: "workflow",
		ResourceID:   workflowID,
		Metadata:     map[string]any{"type": "transcribe-long", "job_id": jobID},
	})

	writeJSON(w, http.StatusCreated, map[string]any{
		"id":             workflowID,
		"type":           "transcribe-long",
		"status":         "queued",
		"current_job_id": jobID,
	})
}

func (h *WorkflowHandler) Get(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFromContext(r.Context())
	id := chi.URLParam(r, "id")
	var wf Workflow
	var paramsBytes []byte
	var resultBytes []byte
	var currentJobID *string
	err := h.DB.WithTenant(r.Context(), p.OrgID, func(ctx context.Context) error {
		return dbtx.QueryRow(ctx, h.DB, `
			SELECT id::text, 'transcribe-long', status, artifact_id::text, params, result, current_job_id::text, COALESCE(error, ''), created_at, updated_at
			FROM workflows WHERE id = $1
		`, id).Scan(&wf.ID, &wf.Type, &wf.Status, &wf.ArtifactID, &paramsBytes, &resultBytes, &currentJobID, &wf.Error, &wf.CreatedAt, &wf.UpdatedAt)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeProblem(w, http.StatusNotFound, "not_found", "Workflow not found")
			return
		}
		writeProblem(w, http.StatusInternalServerError, "internal", "Failed to get workflow")
		return
	}
	wf.Params = paramsBytes
	if resultBytes != nil {
		wf.Result = resultBytes
	}
	wf.CurrentJobID = currentJobID
	writeJSON(w, http.StatusOK, wf)
}

func mergeWorkflowID(params json.RawMessage, workflowID string) []byte {
	if len(params) == 0 {
		return []byte(fmt.Sprintf(`{"_workflow_id": %q}`, workflowID))
	}
	var m map[string]any
	if err := json.Unmarshal(params, &m); err != nil {
		return []byte(fmt.Sprintf(`{"_workflow_id": %q}`, workflowID))
	}
	m["_workflow_id"] = workflowID
	out, _ := json.Marshal(m)
	return out
}
