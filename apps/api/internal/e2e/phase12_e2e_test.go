// Phase 1 & 2 end-to-end through the real API + real worker + NATS + MinIO.
//
// Drives, over HTTP: a convert-to-wav job whose source artifact the worker
// transcodes to 16 kHz mono WAV (proving the new standalone processor, the
// queued→running→completed pipeline over real JetStream, and Phase 2 cost
// attribution), then the dead-letter requeue path (POST /v1/jobs/{id}/requeue
// moves a dead_letter job back to queued). Gated on ORPHEUS_E2E=1 + the
// ORPHEUS_TEST_* service env. Hermetic on 127.0.0.1:18092.
package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/orpheus/api/internal/e2e/setup"
)

const phase12APIPort = 18092

func TestE2E_Phase12(t *testing.T) {
	if os.Getenv("ORPHEUS_E2E") != "1" {
		t.Skip("set ORPHEUS_E2E=1 to run")
	}
	dsn := os.Getenv("ORPHEUS_TEST_DATABASE_URL")
	natsURL := os.Getenv("ORPHEUS_TEST_NATS_URL")
	s3Endpoint := os.Getenv("ORPHEUS_TEST_S3_ENDPOINT")
	s3Bucket := os.Getenv("ORPHEUS_TEST_S3_BUCKET")
	s3AccessKey := os.Getenv("ORPHEUS_TEST_S3_ACCESS_KEY")
	s3SecretKey := os.Getenv("ORPHEUS_TEST_S3_SECRET_KEY")
	if dsn == "" || natsURL == "" || s3Endpoint == "" || s3Bucket == "" || s3AccessKey == "" || s3SecretKey == "" {
		t.Skip("missing ORPHEUS_TEST_{DATABASE,NATS,S3}_* env")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	setup.RunMigrations(t, ctx, dsn) // applies 0017 → seeds convert-to-wav catalog row
	pool := setup.ServicePool(t, ctx, dsn)
	setup.StartWorker(t, ctx, natsURL, dsn, s3Endpoint, s3Bucket, s3AccessKey, s3SecretKey)
	apiURL, apiStop := setup.StartAPI(t, ctx, pool, natsURL, phase12APIPort)
	t.Cleanup(apiStop)

	orgID := uuid.NewString()
	setup.SeedOrg(t, ctx, pool, orgID)
	setup.SeedUser(t, ctx, pool, orgID)
	setup.CleanupOrgData(t, pool, orgID)
	t.Cleanup(func() {
		c, cc := context.WithTimeout(context.Background(), 5*time.Second)
		defer cc()
		_, _ = pool.Exec(c, `DELETE FROM jobs WHERE org_id=$1`, orgID)
		_, _ = pool.Exec(c, `DELETE FROM artifacts WHERE org_id=$1`, orgID)
		_, _ = pool.Exec(c, `DELETE FROM organizations WHERE id=$1`, orgID)
	})

	// Source audio: 48 kHz stereo WAV → convert should yield 16 kHz mono.
	wav := setup.BuildWAV(3, 48000, 2)
	audioKey := "e2e/phase12/source.wav"
	setup.EnsureBucket(t, ctx, s3Endpoint, s3Bucket, s3AccessKey, s3SecretKey)
	setup.UploadS3(t, ctx, s3Endpoint, s3Bucket, audioKey, wav, s3AccessKey, s3SecretKey)
	t.Cleanup(func() { setup.DeleteS3(t, ctx, s3Endpoint, s3Bucket, audioKey, s3AccessKey, s3SecretKey) })
	srcArtifact := setup.SeedArtifact(t, ctx, pool, orgID, s3Bucket, audioKey, len(wav))

	key := seedBroadAPIKey(t, ctx, pool, orgID)

	// A) convert-to-wav through the real worker -------------------------------
	jobID := setup.PostJob(t, ctx, apiURL, key, srcArtifact, "convert-to-wav", "1.0.0", "")
	status, result := setup.WaitForJobComplete(t, ctx, apiURL, key, jobID, 90*time.Second)
	if status != "completed" {
		t.Fatalf("convert-to-wav job status = %s, want completed; result=%s", status, result)
	}
	var conv struct {
		ArtifactID       string `json:"artifact_id"`
		SourceArtifactID string `json:"source_artifact_id"`
		SampleRate       int    `json:"sample_rate"`
		Channels         int    `json:"channels"`
		ContentType      string `json:"content_type"`
		SizeBytes        int64  `json:"size_bytes"`
	}
	if err := json.Unmarshal(result, &conv); err != nil {
		t.Fatalf("decode result: %v (%s)", err, result)
	}
	if conv.SampleRate != 16000 || conv.Channels != 1 || conv.ContentType != "audio/wav" {
		t.Fatalf("convert result = %dHz/%dch/%s, want 16000/1/audio/wav", conv.SampleRate, conv.Channels, conv.ContentType)
	}
	if conv.ArtifactID == "" || conv.SizeBytes <= 0 {
		t.Fatalf("convert produced no artifact: %+v", conv)
	}

	// The output artifact row exists and is audio/wav.
	var outContentType string
	if err := pool.QueryRow(ctx, `SELECT content_type FROM artifacts WHERE id=$1 AND org_id=$2`, conv.ArtifactID, orgID).Scan(&outContentType); err != nil {
		t.Fatalf("output artifact not found: %v", err)
	}
	if outContentType != "audio/wav" {
		t.Fatalf("output artifact content_type = %q, want audio/wav", outContentType)
	}

	// Phase 2 cost attribution: a completed job has a computed positive cost.
	var costUSD float64
	if err := pool.QueryRow(ctx, `SELECT COALESCE(cost_usd,0) FROM jobs WHERE id=$1`, jobID).Scan(&costUSD); err != nil {
		t.Fatalf("read cost: %v", err)
	}
	if costUSD <= 0 {
		t.Fatalf("cost_usd = %v, want > 0 (computed from wall-clock)", costUSD)
	}
	t.Logf("[PASS] Phase 1/2: convert-to-wav → 16kHz mono artifact %s, cost_usd=%v", conv.ArtifactID, costUSD)

	// B) Dead-letter requeue path ---------------------------------------------
	dlJob := uuid.NewString()
	if _, err := pool.Exec(ctx,
		`INSERT INTO jobs (id, org_id, job_type, status, params, artifact_id, attempts, max_retries, created_at)
		 VALUES ($1,$2,'custom','dead_letter','{}'::jsonb,$3,3,3, now())`,
		dlJob, orgID, srcArtifact); err != nil {
		t.Fatalf("seed dead_letter job: %v", err)
	}
	code, body := authReq(t, ctx, http.MethodPost, apiURL+"/v1/jobs/"+dlJob+"/requeue", key, nil)
	if code != http.StatusOK {
		t.Fatalf("requeue = %d, want 200; body=%s", code, body)
	}
	var reqStatus string
	var attempts int
	if err := pool.QueryRow(ctx, `SELECT status::text, attempts FROM jobs WHERE id=$1`, dlJob).Scan(&reqStatus, &attempts); err != nil {
		t.Fatalf("read requeued job: %v", err)
	}
	if reqStatus != "queued" || attempts != 0 {
		t.Fatalf("after requeue: status=%q attempts=%d, want queued/0", reqStatus, attempts)
	}
	t.Logf("[PASS] Phase 2: dead_letter job requeued → queued (attempts reset)")

	t.Log("=== PHASE 1 & 2 E2E PASSED THROUGH THE REAL PIPELINE ===")
}
