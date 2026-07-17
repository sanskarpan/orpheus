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

// TestJobCache_MissThenHit drives the real Create handler: an identical
// cacheable job returns the prior result on the second submit, after the
// worker's cache-population contract runs. RLS is load-bearing throughout.
func TestJobCache_MissThenHit(t *testing.T) {
	sut := testArtifactDB(t) // tenant-scoped (RLS enforced)
	svc := testServiceDB(t)  // service role, seed/populate
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	orgID := uuid.NewString()
	if _, err := svc.Exec(ctx, `INSERT INTO organizations (id,name,slug) VALUES ($1,$2,$2)`, orgID, "cache-"+orgID); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	procID, verID := uuid.NewString(), uuid.NewString()
	procName := "cache-proc-" + procID[:8]
	mvid := "mv-cache-test"
	if _, err := svc.Exec(ctx,
		`INSERT INTO processors (id,name,display_name,tier,timeout_seconds) VALUES ($1,$2,$2,'cpu_tiny',60)`,
		procID, procName); err != nil {
		t.Fatalf("seed processor: %v", err)
	}
	if _, err := svc.Exec(ctx,
		`INSERT INTO processor_versions (id,processor_id,version,model_id,model_version_id,cacheable) VALUES ($1,$2,'1.0.0','m',$3,true)`,
		verID, procID, mvid); err != nil {
		t.Fatalf("seed processor_version: %v", err)
	}
	artID := uuid.NewString()
	if _, err := svc.Exec(ctx,
		`INSERT INTO artifacts (id,org_id,s3_bucket,s3_key,sha256,size_bytes,content_type) VALUES ($1,$2,'b','k',$3,10,'audio/wav')`,
		artID, orgID, "deadbeefcafe1234"); err != nil {
		t.Fatalf("seed artifact: %v", err)
	}
	t.Cleanup(func() {
		cctx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		_, _ = svc.Exec(cctx, `DELETE FROM job_result_cache WHERE org_id=$1`, orgID)
		_, _ = svc.Exec(cctx, `DELETE FROM jobs WHERE org_id=$1`, orgID)
		_, _ = svc.Exec(cctx, `DELETE FROM artifacts WHERE org_id=$1`, orgID)
		_, _ = svc.Exec(cctx, `DELETE FROM processor_versions WHERE id=$1`, verID)
		_, _ = svc.Exec(cctx, `DELETE FROM processors WHERE id=$1`, procID)
		_, _ = svc.Exec(cctx, `DELETE FROM organizations WHERE id=$1`, orgID)
	})

	h := &JobHandler{DB: sut, Audit: audit.New(sut, nil)}
	submit := func(cacheMode, params string) (Job, int) {
		t.Helper()
		body, _ := json.Marshal(CreateJobRequest{
			ArtifactID: artID,
			Processor:  ProcessorRef{Name: procName, Version: "1.0.0"},
			Params:     json.RawMessage(params),
			Cache:      cacheMode,
		})
		req := withPrincipal(httptest.NewRequest(http.MethodPost, "/v1/jobs", bytes.NewReader(body)), &auth.Principal{OrgID: orgID})
		rec := httptest.NewRecorder()
		h.Create(rec, req)
		var j Job
		_ = json.NewDecoder(rec.Body).Decode(&j)
		return j, rec.Code
	}

	// 1) First submit → miss, queued.
	j1, code := submit("auto", `{"lang":"en"}`)
	if code != http.StatusAccepted || j1.Cache != "miss" || j1.Status != "queued" {
		t.Fatalf("first submit = %d cache %q status %q, want 202/miss/queued", code, j1.Cache, j1.Status)
	}

	// 2) Simulate the worker completing job1 and populating the cache — the
	//    exact contract the Python worker runs (copy cache_meta verbatim).
	result := `{"text":"hello world","duration_seconds":1.0}`
	if _, err := svc.Exec(ctx, `UPDATE jobs SET status='completed', result=$2::jsonb, cost_usd=0.5, completed_at=now() WHERE id=$1`, j1.ID, result); err != nil {
		t.Fatalf("complete job1: %v", err)
	}
	if _, err := svc.Exec(ctx, `
		INSERT INTO job_result_cache (org_id, cache_key, input_hash, params_hash, model_version_id, source_job_id, result)
		SELECT org_id, decode(cache_meta->>'ck','hex'), cache_meta->>'ih', cache_meta->>'ph', cache_meta->>'mv', id, $2::jsonb
		FROM jobs WHERE id=$1 AND cache_meta IS NOT NULL
		ON CONFLICT (org_id, cache_key) DO UPDATE SET result=EXCLUDED.result, source_job_id=EXCLUDED.source_job_id
	`, j1.ID, result); err != nil {
		t.Fatalf("populate cache: %v", err)
	}

	// 3) Identical submit → hit, returns the prior result, points at job1.
	j2, code := submit("auto", `{"lang":"en"}`)
	if code != http.StatusOK || j2.Cache != "hit" || j2.Status != "completed" {
		t.Fatalf("second submit = %d cache %q status %q, want 200/hit/completed", code, j2.Cache, j2.Status)
	}
	if j2.CachedFromJobID != j1.ID {
		t.Fatalf("cached_from_job_id = %q, want %q", j2.CachedFromJobID, j1.ID)
	}
	gotText := ""
	if r := map[string]any{}; json.Unmarshal(j2.Result, &r) == nil {
		gotText, _ = r["text"].(string)
	}
	if gotText != "hello world" {
		t.Fatalf("cached result text = %q, want 'hello world'", gotText)
	}
	if j2.CostUSD != 0 {
		t.Fatalf("cache-hit cost = %v, want 0", j2.CostUSD)
	}

	// 4) Key sensitivity: different params must miss even with same input.
	j3, _ := submit("auto", `{"lang":"es"}`)
	if j3.Cache != "miss" {
		t.Fatalf("different params = cache %q, want miss", j3.Cache)
	}

	// 5) cache=only on an uncached key → 409.
	_, code = submit("only", `{"lang":"fr"}`)
	if code != http.StatusConflict {
		t.Fatalf("cache=only miss = %d, want 409", code)
	}

	// 6) Stats reflect at least one hit and entry.
	ch := &CacheHandler{DB: sut}
	srec := httptest.NewRecorder()
	ch.Stats(srec, withPrincipal(httptest.NewRequest(http.MethodGet, "/v1/cache/stats", nil), &auth.Principal{OrgID: orgID}))
	var stats CacheStats
	_ = json.NewDecoder(srec.Body).Decode(&stats)
	if stats.Hits < 1 || stats.Entries < 1 {
		t.Fatalf("stats = %+v, want >=1 hit and entry", stats)
	}
}
