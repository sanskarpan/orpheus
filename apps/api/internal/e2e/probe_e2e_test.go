// Phase 2.3 probe end-to-end smoke.
//
// Proves the full JetStream pipeline for the probe processor: the
// worker subscribes to ORPHEUS_JOBS, downloads a WAV from MinIO,
// runs ffprobe, and writes the parsed metadata back into the
// artifacts row (probe_status, codec, sample_rate, channels,
// duration_seconds) and the jobs row (status, result).
//
// Flow:
//
//	seed org + user + api_key + processor (probe 1.0.0) + artifact
//	upload a 1s 8kHz mono WAV to MinIO at artifact.s3_key
//	start the Go API in-process
//	start the Python JetStream worker subprocess (uses ffprobe + boto3)
//	POST /v1/jobs {artifact_id, processor: probe 1.0.0}
//	  → outbox → NATS JetStream → worker → artifacts.* + jobs.status
//	poll GET /v1/jobs/{id} until status = 'completed'
//	assert artifacts.probe_status = 'completed' and the metadata
//	assert jobs.result has codec / sample_rate / channels / duration_seconds
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
// Hermetic on 127.0.0.1:18083 — distinct from TestE2E_ExtractMetadata
// (18082) and TestE2E_Health (18080).
package e2e

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/orpheus/api/internal/e2e/setup"
)

func TestE2E_Probe(t *testing.T) {
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
	apiURL, apiStop := setup.StartAPI(t, ctx, pool, natsURL, setup.APIPortProbe)
	t.Cleanup(apiStop)

	orgID := uuid.NewString()
	setup.SeedOrg(t, ctx, pool, orgID)
	setup.SeedUser(t, ctx, pool, orgID)
	setup.CleanupOrgData(t, pool, orgID)
	processorID, processorVersionID := setup.SeedProcessor(t, ctx, pool, "probe", "1.0.0")

	s3Key := "e2e/probe/test.wav"
	wav := setup.BuildWAV(1, 8000, 1)
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
	jobID := setup.PostJob(t, ctx, apiURL, apiKey, artifactID, "probe", "1.0.0", "")
	status, resultRaw := setup.WaitForJobComplete(t, ctx, apiURL, apiKey, jobID, setup.WaitJob)

	if status != "completed" {
		t.Fatalf("job %s status = %s, want completed; result = %s", jobID, status, string(resultRaw))
	}

	var probeStatus, codec string
	var sampleRate, channels int
	var durationSeconds float64
	if err := pool.QueryRow(ctx,
		`SELECT probe_status, codec, sample_rate, channels, duration_seconds FROM artifacts WHERE id = $1`,
		artifactID,
	).Scan(&probeStatus, &codec, &sampleRate, &channels, &durationSeconds); err != nil {
		t.Fatalf("query artifact: %v", err)
	}
	if probeStatus != "completed" {
		t.Errorf("artifact.probe_status = %q, want completed", probeStatus)
	}
	if codec != "pcm_s16le" {
		t.Errorf("artifact.codec = %q, want pcm_s16le", codec)
	}
	if sampleRate != 8000 {
		t.Errorf("artifact.sample_rate = %d, want 8000", sampleRate)
	}
	if channels != 1 {
		t.Errorf("artifact.channels = %d, want 1", channels)
	}
	if durationSeconds < 0.9 || durationSeconds > 1.1 {
		t.Errorf("artifact.duration_seconds = %v, want value in (0.9, 1.1)", durationSeconds)
	}

	var result map[string]any
	if err := json.Unmarshal(resultRaw, &result); err != nil {
		t.Fatalf("decode result: %v (raw=%s)", err, string(resultRaw))
	}
	for _, k := range []string{"codec", "sample_rate", "channels", "duration_seconds"} {
		if _, ok := result[k]; !ok {
			t.Errorf("result missing key %q: %s", k, string(resultRaw))
		}
	}
}
