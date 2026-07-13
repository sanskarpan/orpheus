// Phase 2.4 slice end-to-end smoke.
//
// Proves the full JetStream pipeline for the slice processor: the
// worker subscribes to ORPHEUS_JOBS, downloads a WAV from MinIO,
// runs ffmpeg -ss/-to -c copy to extract a time range, uploads the
// slice to a new s3_key under slices/{org}/{src}/, inserts a new
// artifacts row, and writes the slice metadata back into jobs.result.
//
// Flow:
//
//	seed org + user + api_key + processor (slice 1.0.0) + artifact
//	upload a 2s 8kHz mono WAV to MinIO at artifact.s3_key
//	start the Go API in-process
//	start the Python JetStream worker subprocess (uses ffmpeg + boto3)
//	POST /v1/jobs {artifact_id, processor: slice 1.0.0, params: {start_seconds:0.5, end_seconds:1.5}}
//	  → outbox → NATS JetStream → worker → artifacts.* + jobs.status
//	poll GET /v1/jobs/{id} until status = 'completed'
//	assert result.slice_artifact_id / start_seconds / end_seconds
//	assert the new artifacts row points at slices/{org}/{src}/ and is ~half the source size
//	assert probe_status defaults to 'pending' (slice does not probe its own output)
//
// Gated on:
//
//	ORPHEUS_E2E=1
//	ORPHEUS_TEST_DATABASE_URL
//	ORPHEUS_TEST_NATS_URL
//	ORPHEUS_TEST_S3_ENDPOINT
//	ORPHEUS_TEST_S3_BUCKET
//	ORPHEUS_TEST_S3_ACCESS_KEY
//	ORPHEUS_TEST_S3_SECRET_KEY
//
// Hermetic on 127.0.0.1:18084 — distinct from TestE2E_ExtractMetadata
// (18082) and TestE2E_Probe (18083) and TestE2E_Health (18080).
package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/orpheus/api/internal/e2e/setup"
)

func TestE2E_Slice(t *testing.T) {
	if os.Getenv("ORPHEUS_E2E") != "1" {
		t.Skip("set ORPHEUS_E2E=1 to run")
	}
	dsn := os.Getenv("ORPHEUS_TEST_DATABASE_URL")
	natsURL := os.Getenv("ORPHEUS_TEST_NATS_URL")
	s3Endpoint := os.Getenv("ORPHEUS_TEST_S3_ENDPOINT")
	s3Bucket := os.Getenv("ORPHEUS_TEST_S3_BUCKET")
	s3AccessKey := os.Getenv("ORPHEUS_TEST_S3_ACCESS_KEY")
	s3SecretKey := os.Getenv("ORPHEUS_TEST_S3_SECRET_KEY")
	if dsn == "" || natsURL == "" ||
		s3Endpoint == "" || s3Bucket == "" ||
		s3AccessKey == "" || s3SecretKey == "" {
		t.Skip("missing one of ORPHEUS_TEST_{DATABASE,NATS,S3}_*_URL")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	setup.RunMigrations(t, ctx, dsn)
	pool := setup.ServicePool(t, ctx, dsn)
	setup.StartWorker(t, ctx, natsURL, dsn, s3Endpoint, s3Bucket, s3AccessKey, s3SecretKey)
	apiURL, apiStop := setup.StartAPI(t, ctx, pool, natsURL, 18084)
	t.Cleanup(apiStop)

	orgID := uuid.NewString()
	setup.SeedOrg(t, ctx, pool, orgID)
	setup.SeedUser(t, ctx, pool, orgID)
	setup.CleanupOrgData(t, pool, orgID)
	processorID, processorVersionID := setup.SeedProcessor(t, ctx, pool, "slice", "1.0.0")

	s3Key := "e2e/slice/test.wav"
	wav := setup.BuildWAV(2, 8000, 1)
	setup.EnsureBucket(t, ctx, s3Endpoint, s3Bucket, s3AccessKey, s3SecretKey)
	setup.UploadS3(t, ctx, s3Endpoint, s3Bucket, s3Key, wav, s3AccessKey, s3SecretKey)
	t.Cleanup(func() {
		setup.DeleteS3(t, ctx, s3Endpoint, s3Bucket, s3Key, s3AccessKey, s3SecretKey)
	})
	artifactID := setup.SeedArtifact(t, ctx, pool, orgID, s3Bucket, s3Key, len(wav))
	t.Cleanup(func() {
		cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanCancel()
		_, _ = pool.Exec(cleanCtx, `DELETE FROM artifacts WHERE id = $1`, artifactID)
		_, _ = pool.Exec(cleanCtx, `DELETE FROM processor_versions WHERE id = $1`, processorVersionID)
		_, _ = pool.Exec(cleanCtx, `DELETE FROM processors WHERE id = $1`, processorID)
	})

	apiKey := setup.SeedAPIKey(t, ctx, pool, orgID)
	jobID := setup.PostJob(t, ctx, apiURL, apiKey, artifactID, "slice", "1.0.0",
		`{"start_seconds": 0.5, "end_seconds": 1.5}`)
	status, resultRaw := setup.WaitForJobComplete(t, ctx, apiURL, apiKey, jobID, setup.WaitJob)

	if status != "completed" {
		t.Fatalf("job %s status = %s, want completed; result = %s", jobID, status, string(resultRaw))
	}

	var result struct {
		SliceArtifactID  string  `json:"slice_artifact_id"`
		SourceArtifactID string  `json:"source_artifact_id"`
		StartSeconds     float64 `json:"start_seconds"`
		EndSeconds       float64 `json:"end_seconds"`
		SizeBytes        int64   `json:"size_bytes"`
	}
	if err := json.Unmarshal(resultRaw, &result); err != nil {
		t.Fatalf("decode result: %v (raw=%s)", err, string(resultRaw))
	}
	if result.SliceArtifactID == "" {
		t.Fatalf("result.slice_artifact_id is empty; raw=%s", string(resultRaw))
	}
	if result.SourceArtifactID != artifactID {
		t.Errorf("result.source_artifact_id = %q, want %q", result.SourceArtifactID, artifactID)
	}
	if result.StartSeconds != 0.5 {
		t.Errorf("result.start_seconds = %v, want 0.5", result.StartSeconds)
	}
	if result.EndSeconds != 1.5 {
		t.Errorf("result.end_seconds = %v, want 1.5", result.EndSeconds)
	}

	var sliceKey, sliceContentType, sliceProbeStatus string
	var sliceSize int64
	if err := pool.QueryRow(ctx,
		`SELECT s3_key, size_bytes, content_type, probe_status::text FROM artifacts WHERE id = $1`,
		result.SliceArtifactID,
	).Scan(&sliceKey, &sliceSize, &sliceContentType, &sliceProbeStatus); err != nil {
		t.Fatalf("query slice artifact: %v", err)
	}

	wantPrefix := fmt.Sprintf("slices/%s/%s/", orgID, artifactID)
	if !strings.HasPrefix(sliceKey, wantPrefix) {
		t.Errorf("slice s3_key = %q, want prefix %q", sliceKey, wantPrefix)
	}
	if sliceContentType != "audio/wav" {
		t.Errorf("slice content_type = %q, want audio/wav", sliceContentType)
	}
	if sliceSize < 14000 || sliceSize > 22000 {
		t.Errorf("slice size_bytes = %d, want value in (14000, 22000); half of 2s 8kHz 16-bit mono WAV + header", sliceSize)
	}
	if sliceProbeStatus != "pending" {
		t.Errorf("slice probe_status = %q, want pending", sliceProbeStatus)
	}

	t.Cleanup(func() {
		cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanCancel()
		_, _ = pool.Exec(cleanCtx, `DELETE FROM artifacts WHERE id = $1`, result.SliceArtifactID)
		setup.DeleteS3(t, cleanCtx, s3Endpoint, s3Bucket, sliceKey, s3AccessKey, s3SecretKey)
	})
}
