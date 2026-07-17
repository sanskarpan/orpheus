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

// TestUploads_ResumableAndURLIngest drives PRD 09: URL-ingest creates a
// url-source session + job (and SSRF-blocks a bad URL), and the resumable
// endpoints report uploaded/missing parts from a real MinIO multipart upload.
func TestUploads_ResumableAndURLIngest(t *testing.T) {
	sut := testArtifactDB(t)
	svc := testServiceDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	s3c, err := s3.New(ctx, &config.Config{
		S3Endpoint: "http://127.0.0.1:9000", S3AccessKey: "orpheus",
		S3SecretKey: "orpheus-dev-secret", S3Bucket: "orpheus-uploads",
	})
	if err != nil {
		t.Skipf("s3 unavailable: %v", err)
	}

	orgID := uuid.NewString()
	if _, err := svc.Exec(ctx, `INSERT INTO organizations (id,name,slug) VALUES ($1,$2,$2)`, orgID, "up-"+orgID); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	t.Cleanup(func() {
		c, cc := context.WithTimeout(context.Background(), 5*time.Second)
		defer cc()
		_, _ = svc.Exec(c, `DELETE FROM jobs WHERE org_id=$1`, orgID)
		_, _ = svc.Exec(c, `DELETE FROM artifacts WHERE org_id=$1`, orgID)
		_, _ = svc.Exec(c, `DELETE FROM upload_sessions WHERE org_id=$1`, orgID)
		_, _ = svc.Exec(c, `DELETE FROM organizations WHERE id=$1`, orgID)
	})

	h := &UploadHandler{DB: sut, S3: s3c, Audit: audit.New(sut, nil)}
	princ := &auth.Principal{OrgID: orgID}

	// 1) URL ingest — SSRF block on a non-https URL.
	bad := httptest.NewRecorder()
	h.CreateURLIngest(bad, withPrincipal(httptest.NewRequest(http.MethodPost, "/v1/uploads/url", bytes.NewReader([]byte(`{"url":"http://example.com/x.mp3"}`))), princ))
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("ssrf url = %d, want 400", bad.Code)
	}

	// 1b) URL ingest — valid https creates a url-source session + ingest.url job.
	ok := httptest.NewRecorder()
	h.CreateURLIngest(ok, withPrincipal(httptest.NewRequest(http.MethodPost, "/v1/uploads/url", bytes.NewReader([]byte(`{"url":"https://example.com/ep42.mp3","filename":"ep42.mp3","content_type":"audio/mpeg"}`))), princ))
	if ok.Code != http.StatusAccepted {
		t.Fatalf("url ingest = %d: %s", ok.Code, ok.Body.String())
	}
	var ing struct {
		UploadID string `json:"upload_id"`
		Status   string `json:"status"`
	}
	_ = json.NewDecoder(ok.Body).Decode(&ing)
	if ing.Status != "fetching" {
		t.Fatalf("ingest status = %q, want fetching", ing.Status)
	}
	var source, fetchStatus string
	if err := svc.QueryRow(ctx, `SELECT source, COALESCE(fetch_status,'') FROM upload_sessions WHERE id=$1`, ing.UploadID).Scan(&source, &fetchStatus); err != nil {
		t.Fatalf("read session: %v", err)
	}
	if source != "url" || fetchStatus != "fetching" {
		t.Fatalf("session source=%q fetch=%q, want url/fetching", source, fetchStatus)
	}
	var jobCount int
	_ = svc.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE org_id=$1 AND params->'_processor'->>'name'='ingest.url'`, orgID).Scan(&jobCount)
	if jobCount != 1 {
		t.Fatalf("ingest.url jobs = %d, want 1", jobCount)
	}

	// 2) Resumable: a real multipart upload with part 1 uploaded, part 2 missing.
	key := "uploads/" + orgID + "/resumable-test"
	uploadID, err := s3c.CreateMultipartUpload(ctx, key, "audio/wav")
	if err != nil {
		t.Fatalf("create multipart: %v", err)
	}
	t.Cleanup(func() { _ = s3c.AbortMultipartUpload(context.Background(), key, uploadID) })
	// size implies 2 parts (defaultPartSize 8 MiB → 9 MiB = 2).
	sessID := uuid.NewString()
	if _, err := svc.Exec(ctx, `INSERT INTO upload_sessions (id,org_id,filename,content_type,size_bytes,status,expires_at,s3_bucket,s3_key,s3_upload_id) VALUES ($1,$2,'r.wav','audio/wav',$3,'pending',now()+interval '1 hour','orpheus-uploads',$4,$5)`,
		sessID, orgID, int64(9<<20), key, uploadID); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	// Upload part 1 (>=5 MiB so S3/MinIO accepts a non-final part).
	part1URL, err := s3c.Presigner().PresignUploadPart(ctx, "orpheus-uploads", key, uploadID, 1)
	if err != nil {
		t.Fatalf("presign part: %v", err)
	}
	body := bytes.Repeat([]byte("x"), 5<<20)
	putReq, _ := http.NewRequestWithContext(ctx, http.MethodPut, part1URL, bytes.NewReader(body))
	putResp, err := http.DefaultClient.Do(putReq)
	if err != nil {
		t.Fatalf("put part: %v", err)
	}
	_ = putResp.Body.Close()
	if putResp.StatusCode != http.StatusOK {
		t.Fatalf("put part status = %d", putResp.StatusCode)
	}

	router := chi.NewRouter()
	router.Get("/v1/uploads/{id}/parts", func(w http.ResponseWriter, r *http.Request) { h.GetParts(w, withPrincipal(r, princ)) })
	router.Post("/v1/uploads/{id}/parts:refresh", func(w http.ResponseWriter, r *http.Request) { h.RefreshParts(w, withPrincipal(r, princ)) })

	prec := httptest.NewRecorder()
	router.ServeHTTP(prec, httptest.NewRequest(http.MethodGet, "/v1/uploads/"+sessID+"/parts", nil))
	if prec.Code != http.StatusOK {
		t.Fatalf("get parts = %d: %s", prec.Code, prec.Body.String())
	}
	var parts struct {
		Uploaded []struct {
			PartNumber int `json:"part_no"`
		} `json:"uploaded"`
		Missing []int `json:"missing"`
	}
	_ = json.NewDecoder(prec.Body).Decode(&parts)
	if len(parts.Uploaded) != 1 || parts.Uploaded[0].PartNumber != 1 {
		t.Fatalf("uploaded = %+v, want [1]", parts.Uploaded)
	}
	if len(parts.Missing) != 1 || parts.Missing[0] != 2 {
		t.Fatalf("missing = %v, want [2]", parts.Missing)
	}

	// 3) Refresh the missing part → fresh presigned URL.
	rrec := httptest.NewRecorder()
	router.ServeHTTP(rrec, httptest.NewRequest(http.MethodPost, "/v1/uploads/"+sessID+"/parts:refresh", bytes.NewReader([]byte(`{"part_numbers":[2]}`))))
	if rrec.Code != http.StatusOK {
		t.Fatalf("refresh = %d: %s", rrec.Code, rrec.Body.String())
	}
	var refresh struct {
		Parts []struct {
			PartNumber int    `json:"part_no"`
			URL        string `json:"url"`
		} `json:"parts"`
	}
	_ = json.NewDecoder(rrec.Body).Decode(&refresh)
	if len(refresh.Parts) != 1 || refresh.Parts[0].URL == "" {
		t.Fatalf("refresh parts = %+v", refresh.Parts)
	}
}
