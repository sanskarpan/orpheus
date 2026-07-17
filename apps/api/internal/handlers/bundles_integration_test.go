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
	"github.com/orpheus/api/internal/config"
	"github.com/orpheus/api/internal/storage/s3"
)

// TestBundle_CreateResolveDownloadRevoke drives the bundle lifecycle against a
// live database: create (resolving an artifact + a job's result under RLS),
// simulate the worker finishing, download (302), list, revoke. Cross-tenant
// sources 404. RLS is load-bearing throughout.
func TestBundle_CreateResolveDownloadRevoke(t *testing.T) {
	sut := testArtifactDB(t)
	svc := testServiceDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	orgID := uuid.NewString()
	otherOrg := uuid.NewString()
	for _, o := range []string{orgID, otherOrg} {
		if _, err := svc.Exec(ctx, `INSERT INTO organizations (id,name,slug) VALUES ($1,$2,$2)`, o, "bnd-"+o); err != nil {
			t.Fatalf("seed org: %v", err)
		}
	}
	artID := uuid.NewString()
	otherArtID := uuid.NewString()
	if _, err := svc.Exec(ctx, `INSERT INTO artifacts (id,org_id,s3_bucket,s3_key,sha256,size_bytes,content_type) VALUES ($1,$2,'b','k/audio.wav',$3,10,'audio/wav')`, artID, orgID, "sha-a"); err != nil {
		t.Fatalf("seed artifact: %v", err)
	}
	if _, err := svc.Exec(ctx, `INSERT INTO artifacts (id,org_id,s3_bucket,s3_key,sha256,size_bytes,content_type) VALUES ($1,$2,'b','k/other.wav',$3,10,'audio/wav')`, otherArtID, otherOrg, "sha-b"); err != nil {
		t.Fatalf("seed other artifact: %v", err)
	}
	jobID := uuid.NewString()
	if _, err := svc.Exec(ctx, `INSERT INTO jobs (id,org_id,artifact_id,job_type,status,result,cost_usd) VALUES ($1,$2,$3,'transcribe'::job_type,'completed'::job_status,'{"text":"hello"}'::jsonb,0.1)`, jobID, orgID, artID); err != nil {
		t.Fatalf("seed job: %v", err)
	}
	t.Cleanup(func() {
		cctx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		_, _ = svc.Exec(cctx, `DELETE FROM bundle_items WHERE org_id IN ($1,$2)`, orgID, otherOrg)
		_, _ = svc.Exec(cctx, `DELETE FROM bundles WHERE org_id IN ($1,$2)`, orgID, otherOrg)
		_, _ = svc.Exec(cctx, `DELETE FROM jobs WHERE org_id IN ($1,$2)`, orgID, otherOrg)
		_, _ = svc.Exec(cctx, `DELETE FROM artifacts WHERE org_id IN ($1,$2)`, orgID, otherOrg)
		_, _ = svc.Exec(cctx, `DELETE FROM organizations WHERE id IN ($1,$2)`, orgID, otherOrg)
	})

	// Optional S3 client for the download presign (MinIO). Nil is fine — the
	// download sub-check is skipped when it can't be built.
	var s3c *s3.Client
	if c, err := s3.New(ctx, &config.Config{
		S3Endpoint: "http://localhost:9000", S3AccessKey: "orpheus",
		S3SecretKey: "orpheus-dev-secret", S3Bucket: "orpheus-uploads",
	}); err == nil {
		s3c = c
	}
	h := &BundleHandler{DB: sut, S3: s3c, Audit: audit.New(sut, nil)}

	// 1) Create with an artifact + a job-result source.
	body, _ := json.Marshal(CreateBundleRequest{
		Name:    "my-bundle",
		Sources: []BundleSource{{ArtifactID: artID}, {JobID: jobID}},
	})
	rec := httptest.NewRecorder()
	h.Create(rec, withPrincipal(httptest.NewRequest(http.MethodPost, "/v1/bundles", bytes.NewReader(body)), &auth.Principal{OrgID: orgID}))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	var created BundleView
	_ = json.NewDecoder(rec.Body).Decode(&created)
	if created.Status != "building" || created.ArtifactCount != 2 {
		t.Fatalf("created = %+v, want building/2", created)
	}

	// The bundle recorded one item + one result doc, and enqueued the job.
	var itemCount, docCount int
	if err := svc.QueryRow(ctx, `SELECT (SELECT COUNT(*) FROM bundle_items WHERE bundle_id=$1), (SELECT COUNT(*) FROM jsonb_object_keys(result_docs)) FROM bundles WHERE id=$1`, created.ID).Scan(&itemCount, &docCount); err != nil {
		t.Fatalf("counts: %v", err)
	}
	if itemCount != 1 || docCount != 1 {
		t.Fatalf("items=%d docs=%d, want 1/1", itemCount, docCount)
	}
	var jobCount int
	_ = svc.QueryRow(ctx, `SELECT COUNT(*) FROM jobs WHERE org_id=$1 AND params->'_processor'->>'name'='export.bundle'`, orgID).Scan(&jobCount)
	if jobCount != 1 {
		t.Fatalf("export.bundle jobs = %d, want 1", jobCount)
	}

	// 2) Cross-tenant source → 404.
	body2, _ := json.Marshal(CreateBundleRequest{Sources: []BundleSource{{ArtifactID: otherArtID}}})
	rec2 := httptest.NewRecorder()
	h.Create(rec2, withPrincipal(httptest.NewRequest(http.MethodPost, "/v1/bundles", bytes.NewReader(body2)), &auth.Principal{OrgID: orgID}))
	if rec2.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant create = %d, want 404", rec2.Code)
	}

	// 3) Simulate the worker finishing the zip.
	if _, err := svc.Exec(ctx, `UPDATE bundles SET status='ready', s3_bucket='orpheus-uploads', s3_key=$2, size_bytes=1234, artifact_count=2, updated_at=now() WHERE id=$1`, created.ID, "bundles/"+orgID+"/"+created.ID+".zip"); err != nil {
		t.Fatalf("mark ready: %v", err)
	}

	get := func(id string) (BundleView, int) {
		r := chi.NewRouter()
		r.Get("/v1/bundles/{id}", func(w http.ResponseWriter, req *http.Request) {
			h.Get(w, withPrincipal(req, &auth.Principal{OrgID: orgID}))
		})
		rc := httptest.NewRecorder()
		r.ServeHTTP(rc, httptest.NewRequest(http.MethodGet, "/v1/bundles/"+id, nil))
		var bv BundleView
		_ = json.NewDecoder(rc.Body).Decode(&bv)
		return bv, rc.Code
	}
	bv, code := get(created.ID)
	if code != http.StatusOK || bv.Status != "ready" || bv.SizeBytes != 1234 {
		t.Fatalf("get = %d %+v, want ready/1234", code, bv)
	}

	// 4) Download → 302 (only if S3 client was constructed).
	if s3c != nil {
		r := chi.NewRouter()
		r.Get("/v1/bundles/{id}/download", func(w http.ResponseWriter, req *http.Request) {
			h.Download(w, withPrincipal(req, &auth.Principal{OrgID: orgID}))
		})
		rc := httptest.NewRecorder()
		r.ServeHTTP(rc, httptest.NewRequest(http.MethodGet, "/v1/bundles/"+created.ID+"/download", nil))
		if rc.Code != http.StatusFound || rc.Header().Get("Location") == "" {
			t.Fatalf("download = %d loc=%q, want 302 with Location", rc.Code, rc.Header().Get("Location"))
		}
	}

	// 5) List includes the bundle.
	lrec := httptest.NewRecorder()
	h.List(lrec, withPrincipal(httptest.NewRequest(http.MethodGet, "/v1/bundles", nil), &auth.Principal{OrgID: orgID}))
	var list struct {
		Data []BundleView `json:"data"`
	}
	_ = json.NewDecoder(lrec.Body).Decode(&list)
	if len(list.Data) < 1 {
		t.Fatalf("list empty")
	}

	// 6) Revoke → 204, status revoked, download now 409.
	dr := chi.NewRouter()
	dr.Delete("/v1/bundles/{id}", func(w http.ResponseWriter, req *http.Request) {
		h.Delete(w, withPrincipal(req, &auth.Principal{OrgID: orgID}))
	})
	drec := httptest.NewRecorder()
	dr.ServeHTTP(drec, httptest.NewRequest(http.MethodDelete, "/v1/bundles/"+created.ID, nil))
	if drec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d, want 204", drec.Code)
	}
	if bv, _ := get(created.ID); bv.Status != "revoked" {
		t.Fatalf("after revoke status = %q, want revoked", bv.Status)
	}
}
