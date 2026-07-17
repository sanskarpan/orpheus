package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/orpheus/api/internal/audit"
	"github.com/orpheus/api/internal/auth"
	"github.com/orpheus/api/internal/delivery"
)

// TestBatch_CreateDestinationVerify drives the batch + destination API against
// a live database (+ MinIO for the destination verify probe).
func TestBatch_CreateDestinationVerify(t *testing.T) {
	sut := testArtifactDB(t)
	svc := testServiceDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	orgID := uuid.NewString()
	if _, err := svc.Exec(ctx, `INSERT INTO organizations (id,name,slug) VALUES ($1,$2,$2)`, orgID, "bt-"+orgID); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	procID, verID := uuid.NewString(), uuid.NewString()
	procName := "batch-proc-" + procID[:8]
	if _, err := svc.Exec(ctx, `INSERT INTO processors (id,name,display_name,tier,timeout_seconds) VALUES ($1,$2,$2,'cpu_tiny',60)`, procID, procName); err != nil {
		t.Fatalf("seed proc: %v", err)
	}
	if _, err := svc.Exec(ctx, `INSERT INTO processor_versions (id,processor_id,version,model_id,model_version_id) VALUES ($1,$2,'1.0.0','m','mv')`, verID, procID); err != nil {
		t.Fatalf("seed ver: %v", err)
	}
	art1, art2 := uuid.NewString(), uuid.NewString()
	for _, a := range []string{art1, art2} {
		if _, err := svc.Exec(ctx, `INSERT INTO artifacts (id,org_id,s3_bucket,s3_key,sha256,size_bytes,content_type) VALUES ($1,$2,'b',$4,$3,10,'audio/wav')`, a, orgID, "sha-"+a[:8], "k/"+a[:8]); err != nil {
			t.Fatalf("seed artifact: %v", err)
		}
	}
	t.Cleanup(func() {
		c, cc := context.WithTimeout(context.Background(), 5*time.Second)
		defer cc()
		_, _ = svc.Exec(c, `DELETE FROM jobs WHERE org_id=$1`, orgID)
		_, _ = svc.Exec(c, `DELETE FROM batches WHERE org_id=$1`, orgID)
		_, _ = svc.Exec(c, `DELETE FROM delivery_destinations WHERE org_id=$1`, orgID)
		_, _ = svc.Exec(c, `DELETE FROM artifacts WHERE org_id=$1`, orgID)
		_, _ = svc.Exec(c, `DELETE FROM processor_versions WHERE id=$1`, verID)
		_, _ = svc.Exec(c, `DELETE FROM processors WHERE id=$1`, procID)
		_, _ = svc.Exec(c, `DELETE FROM organizations WHERE id=$1`, orgID)
	})

	deliverer := &delivery.Deliverer{StaticEndpoint: "http://127.0.0.1:9000", StaticAccessKey: "orpheus", StaticSecretKey: "orpheus-dev-secret"}
	dh := &DestinationHandler{DB: sut, Audit: audit.New(sut, nil), Deliverer: deliverer}
	bh := &BatchHandler{DB: sut, Audit: audit.New(sut, nil)}

	post := func(h http.HandlerFunc, path string, body string) *httptest.ResponseRecorder {
		req := withPrincipal(httptest.NewRequest(http.MethodPost, path, bytes.NewReader([]byte(body))), &auth.Principal{OrgID: orgID})
		rec := httptest.NewRecorder()
		h(rec, req)
		return rec
	}

	// 1) Create a static (MinIO) destination.
	drec := post(dh.Create, "/v1/destinations",
		`{"type":"s3_static","bucket":"orpheus-uploads","prefix":"tenant-x/","endpoint":"http://127.0.0.1:9000"}`)
	if drec.Code != http.StatusCreated {
		t.Fatalf("destination create = %d: %s", drec.Code, drec.Body.String())
	}
	var dest DestinationView
	_ = json.NewDecoder(drec.Body).Decode(&dest)
	if dest.ExternalID == "" {
		t.Fatal("external_id not generated")
	}

	// 2) Verify → writes+deletes a probe object in MinIO.
	vr := chi.NewRouter()
	vr.Post("/v1/destinations/{id}/verify", func(w http.ResponseWriter, r *http.Request) {
		dh.Verify(w, withPrincipal(r, &auth.Principal{OrgID: orgID}))
	})
	vrec := httptest.NewRecorder()
	vr.ServeHTTP(vrec, httptest.NewRequest(http.MethodPost, "/v1/destinations/"+dest.ID+"/verify", nil))
	var vres map[string]any
	_ = json.NewDecoder(vrec.Body).Decode(&vres)
	if vrec.Code != http.StatusOK || vres["verified"] != true {
		t.Fatalf("verify = %d %v", vrec.Code, vres)
	}

	// 3) Create a batch of 2 jobs targeting the destination.
	brec := post(bh.Create, "/v1/batches", `{"name":"nightly","jobs":[`+
		`{"artifact_id":"`+art1+`","processor":{"name":"`+procName+`","version":"1.0.0"}},`+
		`{"artifact_id":"`+art2+`","processor":{"name":"`+procName+`","version":"1.0.0"}}],`+
		`"delivery":{"destination_id":"`+dest.ID+`"}}`)
	if brec.Code != http.StatusAccepted {
		t.Fatalf("batch create = %d: %s", brec.Code, brec.Body.String())
	}
	var batch BatchView
	_ = json.NewDecoder(brec.Body).Decode(&batch)
	if batch.Status != "running" || batch.JobCount != 2 {
		t.Fatalf("batch = %+v, want running/2", batch)
	}

	// Children exist with batch_id + destination linked.
	var childCount int
	if err := svc.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE batch_id=$1`, batch.ID).Scan(&childCount); err != nil {
		t.Fatalf("count children: %v", err)
	}
	if childCount != 2 {
		t.Fatalf("children=%d, want 2", childCount)
	}

	// 4) Get + ListJobs.
	gr := chi.NewRouter()
	gr.Get("/v1/batches/{id}", func(w http.ResponseWriter, r *http.Request) {
		bh.Get(w, withPrincipal(r, &auth.Principal{OrgID: orgID}))
	})
	gr.Get("/v1/batches/{id}/jobs", func(w http.ResponseWriter, r *http.Request) {
		bh.ListJobs(w, withPrincipal(r, &auth.Principal{OrgID: orgID}))
	})
	grec := httptest.NewRecorder()
	gr.ServeHTTP(grec, httptest.NewRequest(http.MethodGet, "/v1/batches/"+batch.ID, nil))
	var got BatchView
	_ = json.NewDecoder(grec.Body).Decode(&got)
	if got.JobCount != 2 {
		t.Fatalf("get batch job_count=%d", got.JobCount)
	}
	jrec := httptest.NewRecorder()
	gr.ServeHTTP(jrec, httptest.NewRequest(http.MethodGet, "/v1/batches/"+batch.ID+"/jobs", nil))
	var jl struct {
		Data []map[string]any `json:"data"`
	}
	_ = json.NewDecoder(jrec.Body).Decode(&jl)
	if len(jl.Data) != 2 {
		t.Fatalf("list jobs = %d, want 2", len(jl.Data))
	}
}
