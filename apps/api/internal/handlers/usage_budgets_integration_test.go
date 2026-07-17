package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/orpheus/api/internal/audit"
	"github.com/orpheus/api/internal/auth"
)

// TestUsageBudgets_TimeseriesCRUDHardCap drives the PRD 07 API against live
// Postgres: usage timeseries reads the rollup, budgets CRUD reports spend, and
// a hard-cap budget rejects new jobs with 402.
func TestUsageBudgets_TimeseriesCRUDHardCap(t *testing.T) {
	sut := testArtifactDB(t)
	svc := testServiceDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	orgID := uuid.NewString()
	if _, err := svc.Exec(ctx, `INSERT INTO organizations (id,name,slug) VALUES ($1,$2,$2)`, orgID, "ub-"+orgID); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	// Seed rollup rows: total spend 3.0 across 2 days, processor transcribe.
	hour := time.Now().UTC().Truncate(time.Hour)
	for i, c := range []float64{1.0, 2.0} {
		h := hour.AddDate(0, 0, -i)
		if _, err := svc.Exec(ctx, `INSERT INTO usage_rollup_hourly (org_id,hour,dimension,dimension_value,jobs,cost_usd) VALUES ($1,$2,'total','',5,$3),($1,$2,'processor','transcribe',5,$3)`,
			orgID, h, c); err != nil {
			t.Fatalf("seed rollup: %v", err)
		}
	}
	procID, verID := uuid.NewString(), uuid.NewString()
	procName := "hc-proc-" + procID[:8]
	_, _ = svc.Exec(ctx, `INSERT INTO processors (id,name,display_name,tier,timeout_seconds) VALUES ($1,$2,$2,'cpu_tiny',60)`, procID, procName)
	_, _ = svc.Exec(ctx, `INSERT INTO processor_versions (id,processor_id,version,model_id,model_version_id) VALUES ($1,$2,'1.0.0','m','mv')`, verID, procID)
	artID := uuid.NewString()
	_, _ = svc.Exec(ctx, `INSERT INTO artifacts (id,org_id,s3_bucket,s3_key,sha256,size_bytes,content_type) VALUES ($1,$2,'b','k/ub',$3,10,'audio/wav')`, artID, orgID, "sha-ub")
	t.Cleanup(func() {
		c, cc := context.WithTimeout(context.Background(), 5*time.Second)
		defer cc()
		_, _ = svc.Exec(c, `DELETE FROM budgets WHERE org_id=$1`, orgID)
		_, _ = svc.Exec(c, `DELETE FROM usage_rollup_hourly WHERE org_id=$1`, orgID)
		_, _ = svc.Exec(c, `DELETE FROM jobs WHERE org_id=$1`, orgID)
		_, _ = svc.Exec(c, `DELETE FROM artifacts WHERE org_id=$1`, orgID)
		_, _ = svc.Exec(c, `DELETE FROM processor_versions WHERE id=$1`, verID)
		_, _ = svc.Exec(c, `DELETE FROM processors WHERE id=$1`, procID)
		_, _ = svc.Exec(c, `DELETE FROM organizations WHERE id=$1`, orgID)
	})

	princ := &auth.Principal{OrgID: orgID}

	// 1) Timeseries (day granularity, group_by processor).
	uth := &UsageTimeseriesHandler{DB: sut}
	trec := httptest.NewRecorder()
	uth.GetTimeseries(trec, withPrincipal(httptest.NewRequest(http.MethodGet, "/v1/usage/timeseries?granularity=day&group_by=processor", nil), princ))
	if trec.Code != http.StatusOK {
		t.Fatalf("timeseries = %d: %s", trec.Code, trec.Body.String())
	}
	var ts struct {
		Series []map[string]any `json:"series"`
	}
	_ = json.NewDecoder(trec.Body).Decode(&ts)
	if len(ts.Series) < 1 || ts.Series[0]["group"] != "transcribe" {
		t.Fatalf("timeseries series unexpected: %+v", ts.Series)
	}

	// 2) Budget CRUD: create org budget limit 10, list shows spend 3.0.
	bh := &BudgetHandler{DB: sut, Audit: audit.New(sut, nil)}
	brec := httptest.NewRecorder()
	bh.Create(brec, withPrincipal(httptest.NewRequest(http.MethodPost, "/v1/budgets", bytes.NewReader([]byte(`{"scope":"org","limit_usd":10,"enforcement":"alert"}`))), princ))
	if brec.Code != http.StatusCreated {
		t.Fatalf("budget create = %d: %s", brec.Code, brec.Body.String())
	}
	lrec := httptest.NewRecorder()
	bh.List(lrec, withPrincipal(httptest.NewRequest(http.MethodGet, "/v1/budgets", nil), princ))
	var bl struct {
		Data []BudgetView `json:"data"`
	}
	_ = json.NewDecoder(lrec.Body).Decode(&bl)
	if len(bl.Data) != 1 || bl.Data[0].SpendUSD < 2.99 || bl.Data[0].SpendUSD > 3.01 {
		t.Fatalf("budget list spend = %+v, want 3.0", bl.Data)
	}

	// 3) Hard cap: create a hard_cap budget with limit 1.0 (spend 3.0 > 1.0);
	//    submitting a job must 402.
	hrec := httptest.NewRecorder()
	bh.Create(hrec, withPrincipal(httptest.NewRequest(http.MethodPost, "/v1/budgets", bytes.NewReader([]byte(`{"scope":"org","limit_usd":1.0,"enforcement":"hard_cap"}`))), princ))
	if hrec.Code != http.StatusCreated {
		t.Fatalf("hardcap budget create = %d", hrec.Code)
	}
	jh := &JobHandler{DB: sut, Audit: audit.New(sut, nil)}
	body, _ := json.Marshal(CreateJobRequest{ArtifactID: artID, Processor: ProcessorRef{Name: procName, Version: "1.0.0"}})
	jrec := httptest.NewRecorder()
	jh.Create(jrec, withPrincipal(httptest.NewRequest(http.MethodPost, "/v1/jobs", bytes.NewReader(body)), princ))
	if jrec.Code != http.StatusPaymentRequired {
		t.Fatalf("job submit under hard cap = %d, want 402; body=%s", jrec.Code, jrec.Body.String())
	}
}
