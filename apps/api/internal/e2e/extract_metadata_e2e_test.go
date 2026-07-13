// Phase 2 extract-metadata end-to-end smoke.
//
// Proves the full JetStream pipeline: the worker subscribes to
// the ORPHEUS_JOBS stream (subjects adkil.job.>), executes a
// real processor, downloads a WAV from MinIO, runs mutagen on
// it, and writes the result back into the jobs row.
//
// Flow:
//
//	seed org + user + api_key + processor + artifact
//	upload a 1s WAV to MinIO at artifact.s3_key
//	start the Go API in-process
//	start the Python JetStream worker subprocess (uses mutagen + boto3)
//	POST /v1/jobs {artifact_id, processor: extract-metadata}
//	  → outbox → NATS JetStream → worker → jobs.status = 'completed'
//	poll GET /v1/jobs/{id} until status = 'completed'
//	assert result.duration_seconds ≈ 1.0 (±0.1)
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
// MinIO is only in docker-compose (dev); the CI contract job does
// not bring it up, so this test self-skips on CI without breaking
// the build.
//
// Hermetic on 127.0.0.1:18082 — distinct from TestE2E_Probe
// (18083) and TestE2E_PublicSurface (18080).
package e2e

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/orpheus/api/internal/e2e/setup"
)

const (
	emTestProcessorName    = "extract-metadata"
	emTestProcessorVersion = "1.0.0"
	emTestS3Key            = "e2e/extract-metadata/test.wav"
)

func TestE2E_ExtractMetadata(t *testing.T) {
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
	apiURL, apiStop := setup.StartAPI(t, ctx, pool, natsURL, setup.APIPortExtractMetadata)
	t.Cleanup(apiStop)

	orgID := uuid.NewString()
	setup.SeedOrg(t, ctx, pool, orgID)
	setup.SeedUser(t, ctx, pool, orgID)
	setup.CleanupOrgData(t, pool, orgID)
	processorID, processorVersionID := setup.SeedProcessor(t, ctx, pool, emTestProcessorName, emTestProcessorVersion)

	wav := setup.BuildWAV(1, 8000, 1)
	setup.EnsureBucket(t, ctx, s3Endpoint, s3Bucket, s3AccessKey, s3SecretKey)
	setup.UploadS3(t, ctx, s3Endpoint, s3Bucket, emTestS3Key, wav, s3AccessKey, s3SecretKey)
	t.Cleanup(func() {
		setup.DeleteS3(t, ctx, s3Endpoint, s3Bucket, emTestS3Key, s3AccessKey, s3SecretKey)
	})
	artifactID := setup.SeedArtifact(t, ctx, pool, orgID, s3Bucket, emTestS3Key, len(wav))
	t.Cleanup(func() {
		cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanCancel()
		_, _ = pool.Exec(cleanCtx, `DELETE FROM artifacts WHERE id = $1`, artifactID)
		_, _ = pool.Exec(cleanCtx, `DELETE FROM processor_versions WHERE id = $1`, processorVersionID)
		_, _ = pool.Exec(cleanCtx, `DELETE FROM processors WHERE id = $1`, processorID)
	})

	apiKey := setup.SeedAPIKey(t, ctx, pool, orgID)
	jobID := setup.PostJob(t, ctx, apiURL, apiKey, artifactID, emTestProcessorName, emTestProcessorVersion)
	status, resultRaw := setup.WaitForJobComplete(t, ctx, apiURL, apiKey, jobID, setup.WaitJob)

	if status != "completed" {
		t.Fatalf("job %s status = %s, want completed; result = %s", jobID, status, string(resultRaw))
	}

	var result map[string]any
	if err := json.Unmarshal(resultRaw, &result); err != nil {
		t.Fatalf("decode result: %v (raw=%s)", err, string(resultRaw))
	}
	durRaw, ok := result["duration_seconds"]
	if !ok {
		t.Fatalf("result has no duration_seconds key: %s", string(resultRaw))
	}
	dur, ok := durRaw.(float64)
	if !ok {
		t.Fatalf("duration_seconds is %T, want float64", durRaw)
	}
	if math.IsNaN(dur) || dur < 0.9 || dur > 1.1 {
		t.Fatalf("duration_seconds = %v, want value in (0.9, 1.1); full result = %s", dur, string(resultRaw))
	}
}
