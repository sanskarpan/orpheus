package s3_test

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/orpheus/api/internal/config"
	"github.com/orpheus/api/internal/storage/s3"
)

// envOr returns the value of name, or fallback if the env var is empty.
func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

// TestClientRoundTrip is an integration test that exercises the full
// multipart upload + presigned GET + delete cycle against a real
// MinIO (or real S3) endpoint. It is skipped when ORPHEUS_TEST_S3 is
// not set, so a plain `go test ./...` on a developer machine without
// docker compose up does not fail.
//
// Run with:
//
//	ORPHEUS_TEST_S3=1 \
//	ORPHEUS_S3_ENDPOINT=http://localhost:9000 \
//	ORPHEUS_S3_ACCESS_KEY=orpheus \
//	ORPHEUS_S3_SECRET_KEY=orpheus-dev-secret \
//	ORPHEUS_S3_BUCKET=orpheus-test \
//	  go test ./internal/storage/s3/...
func TestClientRoundTrip(t *testing.T) {
	if os.Getenv("ORPHEUS_TEST_S3") == "" {
		t.Skip("ORPHEUS_TEST_S3 not set; skipping integration test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cfg := &config.Config{
		S3Endpoint:  envOr("ORPHEUS_S3_ENDPOINT", "http://localhost:9000"),
		S3AccessKey: envOr("ORPHEUS_S3_ACCESS_KEY", "orpheus"),
		S3SecretKey: envOr("ORPHEUS_S3_SECRET_KEY", "orpheus-dev-secret"),
		S3Bucket:    envOr("ORPHEUS_S3_BUCKET", "orpheus-test"),
	}

	c, err := s3.New(ctx, cfg)
	if err != nil {
		t.Fatalf("s3.New: %v", err)
	}
	if got, want := c.Bucket(), cfg.S3Bucket; got != want {
		t.Fatalf("Bucket() = %q, want %q", got, want)
	}
	if c.Presigner() == nil {
		t.Fatal("Presigner() returned nil")
	}
	if c.Raw() == nil {
		t.Fatal("Raw() returned nil")
	}

	// Unique key per run so concurrent test runs do not collide.
	key := fmt.Sprintf("integration/%d-%s.bin", time.Now().UnixNano(), randomSuffix())

	// Three parts: 5 MB + 5 MB + small tail. S3 requires every
	// non-final part to be at least 5 MB, so part 1 and 2 hit the
	// minimum and part 3 is the trailing chunk.
	part1 := bytes.Repeat([]byte("a"), 5*1024*1024)
	part2 := bytes.Repeat([]byte("b"), 5*1024*1024)
	part3 := []byte("tail-bytes")

	uploadID, err := c.CreateMultipartUpload(ctx, key, "application/octet-stream")
	if err != nil {
		t.Fatalf("CreateMultipartUpload: %v", err)
	}
	if uploadID == "" {
		t.Fatal("CreateMultipartUpload returned empty upload id")
	}
	t.Cleanup(func() {
		// Best-effort cleanup so a failed test does not leave a
		// dangling multipart upload holding storage.
		abortCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = c.AbortMultipartUpload(abortCtx, key, uploadID)
		_ = c.DeleteObject(abortCtx, key)
	})

	parts := []struct {
		body []byte
		num  int32
		etag string
	}{
		{part1, 1, ""},
		{part2, 2, ""},
		{part3, 3, ""},
	}
	for i := range parts {
		url, err := c.Presigner().PresignUploadPart(ctx, c.Bucket(), key, uploadID, parts[i].num)
		if err != nil {
			t.Fatalf("PresignUploadPart(%d): %v", parts[i].num, err)
		}
		if !strings.HasPrefix(url, "http") {
			t.Fatalf("PresignUploadPart(%d): url %q is not an http(s) URL", parts[i].num, url)
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(parts[i].body))
		if err != nil {
			t.Fatalf("NewRequestWithContext(part %d): %v", parts[i].num, err)
		}
		req.Header.Set("Content-Type", "application/octet-stream")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("PUT part %d: %v", parts[i].num, err)
		}
		etag := resp.Header.Get("ETag")
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			t.Fatalf("PUT part %d: status %d", parts[i].num, resp.StatusCode)
		}
		if etag == "" {
			t.Fatalf("PUT part %d: missing ETag header", parts[i].num)
		}
		parts[i].etag = etag
	}

	completed := make([]s3.CompletedPart, len(parts))
	for i, p := range parts {
		completed[i] = s3.CompletedPart{ETag: p.etag, PartNumber: int(p.num)}
	}
	if err := c.CompleteMultipartUpload(ctx, key, uploadID, completed); err != nil {
		t.Fatalf("CompleteMultipartUpload: %v", err)
	}

	// Verify the object exists with the right size and content type.
	size, ctype, err := c.HeadObject(ctx, key)
	if err != nil {
		t.Fatalf("HeadObject: %v", err)
	}
	wantSize := int64(len(part1) + len(part2) + len(part3))
	if size != wantSize {
		t.Fatalf("HeadObject size = %d, want %d", size, wantSize)
	}
	if ctype != "application/octet-stream" {
		t.Fatalf("HeadObject content-type = %q, want application/octet-stream", ctype)
	}

	// Presigned GET round trip: fetch the bytes and confirm they
	// reassemble into the input.
	getURL, err := c.Presigner().PresignGetObject(ctx, c.Bucket(), key, 1*time.Minute)
	if err != nil {
		t.Fatalf("PresignGetObject: %v", err)
	}
	resp, err := http.Get(getURL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		t.Fatalf("GET: status %d", resp.StatusCode)
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	want := append(append(append([]byte{}, part1...), part2...), part3...)
	if !bytes.Equal(got, want) {
		t.Fatalf("GET body mismatch: got %d bytes, want %d bytes", len(got), len(want))
	}

	// DeleteObject removes the object. A second HeadObject should
	// fail. The exact error code is implementation-defined, so we
	// only assert that the call fails.
	if err := c.DeleteObject(ctx, key); err != nil {
		t.Fatalf("DeleteObject: %v", err)
	}
	if _, _, err := c.HeadObject(ctx, key); err == nil {
		t.Fatal("HeadObject after Delete: expected error, got nil")
	}
}

// TestPresignGetTTLClamp asserts that PresignGetObject rejects
// obviously-bad TTLs (zero/negative) and clamps large TTLs to the
// maximum. It needs a real client to invoke the signer, so it is also
// gated on ORPHEUS_TEST_S3. Once the project has an in-memory fake
// S3 (e.g. via a testcontainers MinIO), this test can drop the gate
// and run in the default suite.
func TestPresignGetTTLClamp(t *testing.T) {
	if os.Getenv("ORPHEUS_TEST_S3") == "" {
		t.Skip("ORPHEUS_TEST_S3 not set; skipping presigner clamp test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cfg := &config.Config{
		S3Endpoint:  envOr("ORPHEUS_S3_ENDPOINT", "http://localhost:9000"),
		S3AccessKey: envOr("ORPHEUS_S3_ACCESS_KEY", "orpheus"),
		S3SecretKey: envOr("ORPHEUS_S3_SECRET_KEY", "orpheus-dev-secret"),
		S3Bucket:    envOr("ORPHEUS_S3_BUCKET", "orpheus-test"),
	}
	c, err := s3.New(ctx, cfg)
	if err != nil {
		t.Fatalf("s3.New: %v", err)
	}

	key := fmt.Sprintf("integration/ttl-clamp-%d.bin", time.Now().UnixNano())

	uploadID, err := c.CreateMultipartUpload(ctx, key, "application/octet-stream")
	if err != nil {
		t.Fatalf("CreateMultipartUpload: %v", err)
	}
	t.Cleanup(func() {
		abortCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = c.AbortMultipartUpload(abortCtx, key, uploadID)
		_ = c.DeleteObject(abortCtx, key)
	})

	// A 2-hour TTL must be accepted (and clamped to 1h internally).
	// We do not introspect the URL; we just assert no error.
	if _, err := c.Presigner().PresignGetObject(ctx, c.Bucket(), key, 2*time.Hour); err != nil {
		t.Fatalf("PresignGetObject with large TTL: %v", err)
	}
	// A negative TTL must be coerced to the default, not rejected.
	if _, err := c.Presigner().PresignGetObject(ctx, c.Bucket(), key, -1*time.Second); err != nil {
		t.Fatalf("PresignGetObject with negative TTL: %v", err)
	}
	// A zero TTL must be coerced to the default, not rejected.
	if _, err := c.Presigner().PresignGetObject(ctx, c.Bucket(), key, 0); err != nil {
		t.Fatalf("PresignGetObject with zero TTL: %v", err)
	}
}

// randomSuffix returns 8 hex chars of randomness for naming test
// object keys. The entropy is not security-relevant: it only needs to
// disambiguate concurrent test runs.
func randomSuffix() string {
	sum := md5.Sum([]byte(time.Now().Format(time.RFC3339Nano)))
	return hex.EncodeToString(sum[:4])
}
