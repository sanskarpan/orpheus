package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/orpheus/api/internal/auth"
)

// TestGetUsage_ReflectsJobCost closes the cost-attribution loop: a
// completed job's cost_usd (written by the worker) rolls up into
// GET /v1/usage total_usd for the org.
func TestGetUsage_ReflectsJobCost(t *testing.T) {
	sut := testArtifactDB(t)
	svc := testServiceDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	orgID := uuid.NewString()
	if _, err := svc.Exec(ctx, `INSERT INTO organizations (id,name,slug) VALUES ($1,$2,$2)`, orgID, "cost-"+orgID); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	t.Cleanup(func() {
		cctx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		_, _ = svc.Exec(cctx, `DELETE FROM jobs WHERE org_id=$1`, orgID)
		_, _ = svc.Exec(cctx, `DELETE FROM organizations WHERE id=$1`, orgID)
	})
	if _, err := svc.Exec(ctx, `
		INSERT INTO jobs (id,org_id,job_type,params,status,priority,max_retries,attempts,version,cost_usd,created_at,updated_at,started_at,completed_at)
		VALUES (gen_random_uuid(),$1,'custom'::job_type,'{}'::jsonb,'completed'::job_status,0,3,1,1,0.75,now(),now(),now(),now())
	`, orgID); err != nil {
		t.Fatalf("seed completed job: %v", err)
	}

	h := &SystemHandler{DB: sut}
	req := withPrincipal(httptest.NewRequest(http.MethodGet, "/v1/usage", nil), &auth.Principal{OrgID: orgID})
	rec := httptest.NewRecorder()
	h.GetUsage(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GetUsage = %d, want 200; %s", rec.Code, rec.Body.String())
	}
	var u Usage
	_ = json.NewDecoder(rec.Body).Decode(&u)
	if u.TotalUSD < 0.75 {
		t.Errorf("total_usd = %v, want >= 0.75 (cost not rolled up)", u.TotalUSD)
	}
	if u.JobsCount < 1 {
		t.Errorf("jobs_count = %d, want >= 1", u.JobsCount)
	}
}
