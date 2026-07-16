package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/orpheus/api/internal/auth"
)

// TestJobRequeue_DeadLetterBackToQueued proves the DLQ requeue path: a
// dead-lettered job is returned to 'queued' with a fresh retry budget and
// a job.queued event is re-emitted so a worker picks it up. A non-terminal
// job cannot be requeued (409).
func TestJobRequeue_DeadLetterBackToQueued(t *testing.T) {
	sut := testArtifactDB(t)
	svc := testServiceDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	orgID := uuid.NewString()
	if _, err := svc.Exec(ctx, `INSERT INTO organizations (id,name,slug) VALUES ($1,$2,$2)`, orgID, "rq-"+orgID); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	t.Cleanup(func() {
		cctx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		_, _ = svc.Exec(cctx, `DELETE FROM outbox WHERE org_id=$1`, orgID)
		_, _ = svc.Exec(cctx, `DELETE FROM jobs WHERE org_id=$1`, orgID)
		_, _ = svc.Exec(cctx, `DELETE FROM organizations WHERE id=$1`, orgID)
	})

	jobID := uuid.NewString()
	if _, err := svc.Exec(ctx, `
		INSERT INTO jobs (id, org_id, job_type, params, status, priority, max_retries, attempts, version, created_at, updated_at)
		VALUES ($1,$2,'custom'::job_type,'{"_processor":{"name":"probe","version":"1.0.0"}}'::jsonb,'dead_letter'::job_status,0,3,3,1,now(),now())
	`, jobID, orgID); err != nil {
		t.Fatalf("seed dead_letter job: %v", err)
	}

	h := &JobHandler{DB: sut, Audit: newTestAudit(sut)}
	req := withURLParam(withPrincipal(httptest.NewRequest(http.MethodPost, "/v1/jobs/"+jobID+"/requeue", nil), &auth.Principal{OrgID: orgID}), "id", jobID)
	rec := httptest.NewRecorder()
	h.Requeue(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("Requeue = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// Job is back to queued with attempts reset.
	var status string
	var attempts int
	_ = svc.QueryRow(ctx, `SELECT status::text, attempts FROM jobs WHERE id=$1`, jobID).Scan(&status, &attempts)
	if status != "queued" || attempts != 0 {
		t.Fatalf("after requeue: status=%q attempts=%d, want queued/0", status, attempts)
	}

	// A job.queued event was re-emitted.
	var n int
	_ = svc.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE org_id=$1 AND event_type='job.queued' AND aggregate_id=$2`, orgID, jobID).Scan(&n)
	if n < 1 {
		t.Errorf("expected a re-emitted job.queued outbox row, got %d", n)
	}

	// Requeuing a now-queued (non-terminal) job is a 409.
	req2 := withURLParam(withPrincipal(httptest.NewRequest(http.MethodPost, "/v1/jobs/"+jobID+"/requeue", nil), &auth.Principal{OrgID: orgID}), "id", jobID)
	rec2 := httptest.NewRecorder()
	h.Requeue(rec2, req2)
	if rec2.Code != http.StatusConflict {
		t.Errorf("requeue of a queued job = %d, want 409", rec2.Code)
	}
}
