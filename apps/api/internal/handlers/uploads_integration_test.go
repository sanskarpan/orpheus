package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/orpheus/api/internal/audit"
	"github.com/orpheus/api/internal/auth"
	"github.com/orpheus/api/internal/config"
	"github.com/orpheus/api/internal/db"
	"github.com/orpheus/api/internal/dbtx"
	"github.com/orpheus/api/internal/storage/s3"
)

// TestUploadCreateComplete_RoundTrip is the regression test for the
// bug where Create never persisted s3_bucket/s3_key/s3_upload_id, so
// Complete could never look them up (NULL scan → 500) and no upload
// could ever finish. It drives the real three-step flow against MinIO:
// Create → PUT the part to the presigned URL → Complete, and asserts an
// artifact row is returned.
//
// Gated on ORPHEUS_TEST_S3 (MinIO) and ORPHEUS_TEST_DATABASE_URL.
func TestUploadCreateComplete_RoundTrip(t *testing.T) {
	if os.Getenv("ORPHEUS_TEST_S3") == "" {
		t.Skip("ORPHEUS_TEST_S3 not set; skipping upload round-trip")
	}
	dsn := os.Getenv("ORPHEUS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ORPHEUS_TEST_DATABASE_URL not set; skipping upload round-trip")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cfg := &config.Config{
		S3Endpoint:  envOr("ORPHEUS_S3_ENDPOINT", "http://localhost:9000"),
		S3AccessKey: envOr("ORPHEUS_S3_ACCESS_KEY", "orpheus"),
		S3SecretKey: envOr("ORPHEUS_S3_SECRET_KEY", "orpheus-dev-secret"),
		S3Bucket:    envOr("ORPHEUS_S3_BUCKET", "orpheus-uploads"),
	}
	s3c, err := s3.New(ctx, cfg)
	if err != nil {
		t.Fatalf("s3.New: %v", err)
	}

	// SUT pool is tenant-scoped (no is_service); seed pool is service-role.
	sut := testArtifactDB(t)
	svc := testServiceDB(t)

	orgID := uuid.NewString()
	if _, err := svc.Exec(ctx, `INSERT INTO organizations (id, name, slug) VALUES ($1, $2, $2)`, orgID, "upl-"+orgID); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer ccancel()
		_, _ = svc.Exec(cctx, `DELETE FROM organizations WHERE id = $1`, orgID)
	})

	h := &UploadHandler{DB: sut, S3: s3c, Audit: newTestAudit(sut)}

	// ── Step 1: Create ──────────────────────────────────────────────
	// A real WAV header so the magic-byte gate at Complete accepts it.
	payload := minimalWAV()
	createBody := `{"filename":"clip.wav","content_type":"audio/wav","size_bytes":` + itoa(len(payload)) + `}`
	req := withPrincipal(httptest.NewRequest(http.MethodPost, "/v1/uploads", bytes.NewBufferString(createBody)),
		&auth.Principal{OrgID: orgID})
	rec := httptest.NewRecorder()
	h.Create(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("Create status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var session UploadSession
	if err := json.NewDecoder(rec.Body).Decode(&session); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	if len(session.Parts) != 1 {
		t.Fatalf("want 1 part, got %d", len(session.Parts))
	}

	// Assert the S3 columns were persisted (the crux of the bug).
	var s3Key, s3UploadID string
	if err := sut.WithTenant(ctx, orgID, func(tctx context.Context) error {
		return dbtx.QueryRow(tctx, sut, `SELECT s3_key, s3_upload_id FROM upload_sessions WHERE id = $1`, session.ID).Scan(&s3Key, &s3UploadID)
	}); err != nil {
		t.Fatalf("read session s3 columns: %v", err)
	}
	if s3Key == "" || s3UploadID == "" {
		t.Fatalf("s3_key/s3_upload_id not persisted: key=%q uploadID=%q", s3Key, s3UploadID)
	}
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer ccancel()
		_ = s3c.DeleteObject(cctx, s3Key)
	})

	// ── Step 2: PUT the part to the presigned URL ───────────────────
	putReq, err := http.NewRequestWithContext(ctx, http.MethodPut, session.Parts[0].URL, bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("new PUT req: %v", err)
	}
	putResp, err := http.DefaultClient.Do(putReq)
	if err != nil {
		t.Fatalf("PUT part: %v", err)
	}
	body, _ := io.ReadAll(putResp.Body)
	_ = putResp.Body.Close()
	if putResp.StatusCode != http.StatusOK {
		t.Fatalf("PUT part status = %d; body=%s", putResp.StatusCode, string(body))
	}
	etag := putResp.Header.Get("ETag")
	if etag == "" {
		t.Fatal("PUT part returned no ETag")
	}

	// ── Step 3: Complete ────────────────────────────────────────────
	completeBody, _ := json.Marshal(CompleteUploadRequest{Parts: []CompletedPart{{PartNumber: 1, ETag: etag}}})
	creq := withURLParam(
		withPrincipal(httptest.NewRequest(http.MethodPost, "/v1/uploads/"+session.ID+"/complete", bytes.NewReader(completeBody)),
			&auth.Principal{OrgID: orgID}),
		"id", session.ID)
	crec := httptest.NewRecorder()
	h.Complete(crec, creq)
	if crec.Code != http.StatusCreated && crec.Code != http.StatusOK {
		t.Fatalf("Complete status = %d, want 200/201; body=%s", crec.Code, crec.Body.String())
	}
	var art Artifact
	if err := json.NewDecoder(crec.Body).Decode(&art); err != nil {
		t.Fatalf("decode complete: %v", err)
	}
	if art.ID == "" {
		t.Fatalf("Complete returned no artifact id; body=%s", crec.Body.String())
	}
	if art.SizeBytes != int64(len(payload)) {
		t.Errorf("artifact size = %d, want %d", art.SizeBytes, len(payload))
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func itoa(n int) string {
	return strconv.Itoa(n)
}

func newTestAudit(pool *db.DB) *audit.Recorder {
	return audit.New(pool, nil)
}

// minimalWAV returns a valid 44-byte PCM WAV header + a little data, so
// detectAudioFormat recognises it as audio.
func minimalWAV() []byte {
	buf := new(bytes.Buffer)
	data := make([]byte, 32) // silence
	buf.WriteString("RIFF")
	writeLE32(buf, uint32(36+len(data)))
	buf.WriteString("WAVE")
	buf.WriteString("fmt ")
	writeLE32(buf, 16)   // PCM fmt chunk size
	writeLE16(buf, 1)    // audio format = PCM
	writeLE16(buf, 1)    // channels
	writeLE32(buf, 8000) // sample rate
	writeLE32(buf, 8000) // byte rate
	writeLE16(buf, 1)    // block align
	writeLE16(buf, 8)    // bits per sample
	buf.WriteString("data")
	writeLE32(buf, uint32(len(data)))
	buf.Write(data)
	return buf.Bytes()
}

func writeLE16(b *bytes.Buffer, v uint16) { b.Write([]byte{byte(v), byte(v >> 8)}) }
func writeLE32(b *bytes.Buffer, v uint32) {
	b.Write([]byte{byte(v), byte(v >> 8), byte(v >> 16), byte(v >> 24)})
}

// TestUploadComplete_RejectsNonAudio proves the magic-byte gate: a client
// that passes an audio content_type at Create but uploads non-audio bytes
// is rejected with 415 at Complete, and the object is deleted.
func TestUploadComplete_RejectsNonAudio(t *testing.T) {
	if os.Getenv("ORPHEUS_TEST_S3") == "" {
		t.Skip("ORPHEUS_TEST_S3 not set")
	}
	dsn := os.Getenv("ORPHEUS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ORPHEUS_TEST_DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cfg := &config.Config{
		S3Endpoint:  envOr("ORPHEUS_S3_ENDPOINT", "http://localhost:9000"),
		S3AccessKey: envOr("ORPHEUS_S3_ACCESS_KEY", "orpheus"),
		S3SecretKey: envOr("ORPHEUS_S3_SECRET_KEY", "orpheus-dev-secret"),
		S3Bucket:    envOr("ORPHEUS_S3_BUCKET", "orpheus-uploads"),
	}
	s3c, err := s3.New(ctx, cfg)
	if err != nil {
		t.Fatalf("s3.New: %v", err)
	}
	sut := testArtifactDB(t)
	svc := testServiceDB(t)
	orgID := uuid.NewString()
	if _, err := svc.Exec(ctx, `INSERT INTO organizations (id, name, slug) VALUES ($1,$2,$2)`, orgID, "upl-"+orgID); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	t.Cleanup(func() {
		cctx, c := context.WithTimeout(context.Background(), 10*time.Second)
		defer c()
		_, _ = svc.Exec(cctx, `DELETE FROM organizations WHERE id=$1`, orgID)
	})

	h := &UploadHandler{DB: sut, S3: s3c, Audit: newTestAudit(sut)}

	// Create with an audio content_type (passes the intake gate).
	payload := []byte("<!DOCTYPE html><html>not audio at all</html>")
	body := `{"filename":"evil.wav","content_type":"audio/wav","size_bytes":` + itoa(len(payload)) + `}`
	rec := httptest.NewRecorder()
	h.Create(rec, withPrincipal(httptest.NewRequest(http.MethodPost, "/v1/uploads", bytes.NewBufferString(body)), &auth.Principal{OrgID: orgID}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("Create = %d, want 201; %s", rec.Code, rec.Body.String())
	}
	var session UploadSession
	_ = json.NewDecoder(rec.Body).Decode(&session)

	// Upload the non-audio bytes to the presigned URL.
	putReq, _ := http.NewRequestWithContext(ctx, http.MethodPut, session.Parts[0].URL, bytes.NewReader(payload))
	putResp, err := http.DefaultClient.Do(putReq)
	if err != nil {
		t.Fatalf("PUT: %v", err)
	}
	_ = putResp.Body.Close()
	etag := putResp.Header.Get("ETag")

	// Complete must reject with 415.
	cbody, _ := json.Marshal(CompleteUploadRequest{Parts: []CompletedPart{{PartNumber: 1, ETag: etag}}})
	creq := withURLParam(withPrincipal(httptest.NewRequest(http.MethodPost, "/v1/uploads/"+session.ID+"/complete", bytes.NewReader(cbody)), &auth.Principal{OrgID: orgID}), "id", session.ID)
	crec := httptest.NewRecorder()
	h.Complete(crec, creq)
	if crec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("Complete = %d, want 415 (non-audio should be rejected); body=%s", crec.Code, crec.Body.String())
	}
}
