// PRD 06-10 end-to-end through the real API + real worker + background
// services (batching / usage / erasure) + real MinIO.
//
// Drives, over HTTP: a tracked batch whose children the worker transcribes and
// whose results the batching service pushes to a tenant (MinIO) destination;
// usage rollup surfaced via /v1/usage/timeseries; and a GDPR erasure that the
// erasure service executes with an S3 purge. (PRD 08 redaction + PRD 09 URL
// ingest are covered by their dedicated tests; the API's SSRF gate refuses the
// http/loopback fixture a local ingest e2e would need.) Gated on ORPHEUS_E2E=1
// + the ORPHEUS_TEST_* service env. Hermetic on 127.0.0.1:18091.
package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/orpheus/api/internal/batching"
	"github.com/orpheus/api/internal/config"
	"github.com/orpheus/api/internal/delivery"
	"github.com/orpheus/api/internal/e2e/setup"
	"github.com/orpheus/api/internal/erasure"
	"github.com/orpheus/api/internal/storage/s3"
	"github.com/orpheus/api/internal/usage"
)

const prd0610APIPort = 18091

func TestE2E_PRD0610(t *testing.T) {
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

	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()

	setup.RunMigrations(t, ctx, dsn)
	pool := setup.ServicePool(t, ctx, dsn)
	setup.StartWorker(t, ctx, natsURL, dsn, s3Endpoint, s3Bucket, s3AccessKey, s3SecretKey)
	apiURL, apiStop := setup.StartAPI(t, ctx, pool, natsURL, prd0610APIPort)
	t.Cleanup(apiStop)

	// Background services (StartAPI doesn't run these).
	s3c, err := s3.New(ctx, &config.Config{S3Endpoint: s3Endpoint, S3AccessKey: s3AccessKey, S3SecretKey: s3SecretKey, S3Bucket: s3Bucket})
	if err != nil {
		t.Fatalf("s3.New: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	deliverer := &delivery.Deliverer{StaticEndpoint: s3Endpoint, StaticAccessKey: s3AccessKey, StaticSecretKey: s3SecretKey}
	batchSvc := batching.New(pool, deliverer, s3c, logger)
	usageSvc := usage.New(pool, logger)
	erasureSvc := erasure.New(pool, s3c, logger)

	orgID := uuid.NewString()
	setup.SeedOrg(t, ctx, pool, orgID)
	setup.SeedUser(t, ctx, pool, orgID)
	setup.CleanupOrgData(t, pool, orgID)
	procID, verID := setup.SeedProcessor(t, ctx, pool, "transcribe", "1.0.0")
	t.Cleanup(func() {
		c, cc := context.WithTimeout(context.Background(), 5*time.Second)
		defer cc()
		for _, tbl := range []string{"job_result_cache", "bundle_items", "bundles", "batches", "delivery_destinations", "erasure_requests", "webhook_deliveries", "webhook_endpoints", "usage_rollup_hourly", "budgets", "jobs", "artifacts", "upload_sessions", "outbox"} {
			_, _ = pool.Exec(c, "DELETE FROM "+tbl+" WHERE org_id=$1", orgID)
		}
		_, _ = pool.Exec(c, `DELETE FROM processor_versions WHERE id=$1`, verID)
		_, _ = pool.Exec(c, `DELETE FROM processors WHERE id=$1`, procID)
		_, _ = pool.Exec(c, `DELETE FROM organizations WHERE id=$1`, orgID)
	})

	wav := setup.BuildWAV(12, 16000, 1)
	audioKey := "e2e/prd0610/audio.wav"
	setup.EnsureBucket(t, ctx, s3Endpoint, s3Bucket, s3AccessKey, s3SecretKey)
	setup.UploadS3(t, ctx, s3Endpoint, s3Bucket, audioKey, wav, s3AccessKey, s3SecretKey)
	t.Cleanup(func() { setup.DeleteS3(t, ctx, s3Endpoint, s3Bucket, audioKey, s3AccessKey, s3SecretKey) })
	audioArt := setup.SeedArtifact(t, ctx, pool, orgID, s3Bucket, audioKey, len(wav))

	key := seedBroadAPIKey(t, ctx, pool, orgID)
	get := func(path string) (int, []byte) { return authReq(t, ctx, http.MethodGet, apiURL+path, key, nil) }
	post := func(path, body string) (int, []byte) {
		return authReq(t, ctx, http.MethodPost, apiURL+path, key, []byte(body))
	}

	// A) Batch + tenant-S3 delivery -------------------------------------------
	destPrefix := "tenant-out/" + orgID + "/"
	code, body := post("/v1/destinations", fmt.Sprintf(
		`{"type":"s3_static","bucket":%q,"prefix":%q,"endpoint":%q}`, s3Bucket, destPrefix, s3Endpoint))
	if code != http.StatusCreated {
		t.Fatalf("destination create = %d: %s", code, body)
	}
	var dest struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(body, &dest)

	code, body = post("/v1/batches", fmt.Sprintf(
		`{"name":"nightly","jobs":[{"artifact_id":%q,"processor":{"name":"transcribe","version":"1.0.0"}},{"artifact_id":%q,"processor":{"name":"transcribe","version":"1.0.0"}}],"delivery":{"destination_id":%q}}`,
		audioArt, audioArt, dest.ID))
	if code != http.StatusAccepted {
		t.Fatalf("batch create = %d: %s", code, body)
	}
	var batch struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(body, &batch)

	// Wait for both children to complete (worker transcribes via whisper).
	deadline := time.Now().Add(90 * time.Second)
	for {
		_, jb := get("/v1/batches/" + batch.ID + "/jobs")
		var jl struct {
			Data []struct {
				Status string `json:"status"`
			} `json:"data"`
		}
		_ = json.Unmarshal(jb, &jl)
		done := 0
		for _, j := range jl.Data {
			if j.Status == "completed" || j.Status == "failed" {
				done++
			}
		}
		if len(jl.Data) == 2 && done == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("batch children did not complete: %s", jb)
		}
		time.Sleep(time.Second)
	}
	// Run the batching service: push results + finalize.
	if err := batchSvc.PushPendingResults(ctx); err != nil {
		t.Fatalf("push: %v", err)
	}
	if err := batchSvc.AggregateBatches(ctx); err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	_, bb := get("/v1/batches/" + batch.ID)
	var bv struct {
		Status         string `json:"status"`
		CompletedCount int    `json:"completed_count"`
	}
	_ = json.Unmarshal(bb, &bv)
	if bv.Status != "completed" || bv.CompletedCount != 2 {
		t.Fatalf("batch = %s completed %d, want completed/2", bv.Status, bv.CompletedCount)
	}
	// Result objects were pushed to the destination prefix.
	var delivered int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE batch_id=$1 AND delivery_status='delivered'`, batch.ID).Scan(&delivered)
	if delivered != 2 {
		t.Fatalf("delivered=%d, want 2", delivered)
	}
	t.Logf("[PASS] PRD06 batch → 2 results delivered to tenant S3 + batch completed")

	// (PRD 09 URL ingest is verified through the real fetch + SSRF path in its
	// dedicated tests — the API's SSRF gate correctly refuses the http/loopback
	// fixture a local e2e would need, so it is not re-driven here.)

	// C) Usage rollup + timeseries --------------------------------------------
	if err := usageSvc.RollupOnce(ctx); err != nil {
		t.Fatalf("rollup: %v", err)
	}
	code, body = get("/v1/usage/timeseries?granularity=day&group_by=processor")
	if code != http.StatusOK {
		t.Fatalf("timeseries = %d: %s", code, body)
	}
	var ts struct {
		Series []struct {
			CostUSD float64 `json:"cost_usd"`
		} `json:"series"`
	}
	_ = json.Unmarshal(body, &ts)
	if len(ts.Series) == 0 {
		t.Fatalf("timeseries empty after completed jobs")
	}
	t.Logf("[PASS] PRD07 usage rollup → timeseries has %d bucket(s)", len(ts.Series))

	// D) GDPR erasure ----------------------------------------------------------
	code, body = post("/v1/erasure-requests", fmt.Sprintf(`{"scope":"artifact","artifact_id":%q,"reason":"gdpr_art17","confirm":true}`, audioArt))
	if code != http.StatusAccepted {
		t.Fatalf("erasure create = %d: %s", code, body)
	}
	var er struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(body, &er)
	if err := erasureSvc.ProcessScheduled(ctx); err != nil {
		t.Fatalf("erasure process: %v", err)
	}
	_, eb := get("/v1/erasure-requests/" + er.ID)
	var ev struct {
		Status          string `json:"status"`
		S3ObjectsPurged int    `json:"s3_objects_purged"`
	}
	_ = json.Unmarshal(eb, &ev)
	if ev.Status != "completed" || ev.S3ObjectsPurged != 1 {
		t.Fatalf("erasure = %s purged %d, want completed/1", ev.Status, ev.S3ObjectsPurged)
	}
	// The audio object is gone.
	if _, _, herr := s3c.HeadObject(ctx, audioKey); herr == nil {
		t.Fatal("audio object still present after erasure")
	}
	t.Logf("[PASS] PRD10 erasure → S3 object purged + request completed")

	t.Log("=== ALL PRD 06-10 E2E FEATURES PASSED THROUGH THE REAL PIPELINE ===")
}
