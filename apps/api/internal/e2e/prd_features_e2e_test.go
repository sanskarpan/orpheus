// PRD 01-05 features end-to-end, through the real API + real Python worker.
//
// Drives, over HTTP against the live API (which dispatches to the JetStream
// worker subprocess): transcribe(word_timestamps) → diarize → subtitles →
// translate → summarize, a content cache hit on re-submit, an artifact bundle
// (zip built by the worker, 302 download), and a webhook test-fire that lands
// a recorded delivery attempt. All through the real pipeline — no in-process
// shortcuts.
//
// Gated on ORPHEUS_E2E=1 + the ORPHEUS_TEST_* service env (same as the other
// e2e tests). Hermetic on 127.0.0.1:18090.
package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/alexedwards/argon2id"
	"github.com/google/uuid"

	"github.com/orpheus/api/internal/db"
	"github.com/orpheus/api/internal/e2e/setup"
)

const prdTestAPIPort = 18090

func seedBroadAPIKey(t *testing.T, ctx context.Context, pool *db.DB, orgID string) string {
	t.Helper()
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	secret := "ak_live_" + base64.RawURLEncoding.EncodeToString(b)
	hashed, err := argon2id.CreateHash(secret, argon2id.DefaultParams)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	_, err = pool.Exec(ctx,
		`INSERT INTO api_keys (id, org_id, name, hashed_secret, prefix, scopes) VALUES ($1,$2,'e2e-broad',$3,$4,$5)`,
		uuid.NewString(), orgID, hashed, secret[:9],
		[]string{"jobs:write", "jobs:read", "artifacts:read", "webhooks:write", "webhooks:read", "usage:read"},
	)
	if err != nil {
		t.Fatalf("insert api_key: %v", err)
	}
	return secret
}

func TestE2E_PRDFeatures(t *testing.T) {
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

	setup.RunMigrations(t, ctx, dsn)
	pool := setup.ServicePool(t, ctx, dsn)
	setup.StartWorker(t, ctx, natsURL, dsn, s3Endpoint, s3Bucket, s3AccessKey, s3SecretKey)
	apiURL, apiStop := setup.StartAPI(t, ctx, pool, natsURL, prdTestAPIPort)
	t.Cleanup(apiStop)

	orgID := uuid.NewString()
	setup.SeedOrg(t, ctx, pool, orgID)
	setup.SeedUser(t, ctx, pool, orgID)
	setup.CleanupOrgData(t, pool, orgID)
	procID, verID := setup.SeedProcessor(t, ctx, pool, "transcribe", "1.0.0")
	t.Cleanup(func() {
		c, cc := context.WithTimeout(context.Background(), 5*time.Second)
		defer cc()
		_, _ = pool.Exec(c, `DELETE FROM job_result_cache WHERE org_id=$1`, orgID)
		_, _ = pool.Exec(c, `DELETE FROM bundle_items WHERE org_id=$1`, orgID)
		_, _ = pool.Exec(c, `DELETE FROM bundles WHERE org_id=$1`, orgID)
		_, _ = pool.Exec(c, `DELETE FROM webhook_delivery_attempts WHERE org_id=$1`, orgID)
		_, _ = pool.Exec(c, `DELETE FROM webhook_deliveries WHERE org_id=$1`, orgID)
		_, _ = pool.Exec(c, `DELETE FROM webhook_endpoints WHERE org_id=$1`, orgID)
		_, _ = pool.Exec(c, `DELETE FROM jobs WHERE org_id=$1`, orgID)
		_, _ = pool.Exec(c, `DELETE FROM artifacts WHERE org_id=$1`, orgID)
		_, _ = pool.Exec(c, `DELETE FROM processor_versions WHERE id=$1`, verID)
		_, _ = pool.Exec(c, `DELETE FROM processors WHERE id=$1`, procID)
	})

	// 12s WAV so diarization spans the seeded transcript.
	wav := setup.BuildWAV(12, 16000, 1)
	s3Key := "e2e/prd/audio.wav"
	setup.EnsureBucket(t, ctx, s3Endpoint, s3Bucket, s3AccessKey, s3SecretKey)
	setup.UploadS3(t, ctx, s3Endpoint, s3Bucket, s3Key, wav, s3AccessKey, s3SecretKey)
	t.Cleanup(func() { setup.DeleteS3(t, ctx, s3Endpoint, s3Bucket, s3Key, s3AccessKey, s3SecretKey) })
	audioArt := setup.SeedArtifact(t, ctx, pool, orgID, s3Bucket, s3Key, len(wav))

	key := seedBroadAPIKey(t, ctx, pool, orgID)
	get := func(path string) (int, []byte) { return authReq(t, ctx, http.MethodGet, apiURL+path, key, nil) }
	post := func(path, body string) (int, []byte) {
		return authReq(t, ctx, http.MethodPost, apiURL+path, key, []byte(body))
	}

	// A) Real whisper transcribe with word timestamps.
	tjob := setup.PostJob(t, ctx, apiURL, key, audioArt, "transcribe", "1.0.0", `{"word_timestamps":true}`)
	st, res := setup.WaitForJobComplete(t, ctx, apiURL, key, tjob, setup.WaitJob)
	if st != "completed" {
		t.Fatalf("transcribe status=%s res=%s", st, res)
	}
	assertHasKeys(t, "transcribe", res, "segments", "language", "duration_seconds")
	t.Logf("[PASS] transcribe (real whisper) → %s", trunc(res))

	// B) Seed a rich transcript job (content for the chain) directly.
	transcript := `{"text":"hello there general kenobi you are a bold one now this is podracing",
		"language":"en","segments":[
		{"start":0.0,"end":3.0,"text":"hello there"},
		{"start":3.0,"end":6.0,"text":"general kenobi"},
		{"start":6.0,"end":9.0,"text":"you are a bold one"},
		{"start":9.0,"end":12.0,"text":"now this is podracing"}]}`
	srcJob := uuid.NewString()
	if _, err := pool.Exec(ctx,
		`INSERT INTO jobs (id,org_id,artifact_id,job_type,params,status,result) VALUES ($1,$2,$3,'custom'::job_type,'{}'::jsonb,'completed'::job_status,$4::jsonb)`,
		srcJob, orgID, audioArt, transcript); err != nil {
		t.Fatalf("seed transcript job: %v", err)
	}

	// C) Diarize the transcript (real stub diarizer over the audio).
	djob := setup.PostJob(t, ctx, apiURL, key, audioArt, "audio.diarize", "1.0.0",
		fmt.Sprintf(`{"source_job_id":%q,"max_speakers":2}`, srcJob))
	st, dres := setup.WaitForJobComplete(t, ctx, apiURL, key, djob, setup.WaitJob)
	if st != "completed" {
		t.Fatalf("diarize status=%s res=%s", st, dres)
	}
	var diar struct {
		NumSpeakers int              `json:"num_speakers"`
		Segments    []map[string]any `json:"segments"`
	}
	_ = json.Unmarshal(dres, &diar)
	if diar.NumSpeakers < 1 || len(diar.Segments) != 4 || diar.Segments[0]["speaker"] == nil {
		t.Fatalf("diarize result unexpected: %s", dres)
	}
	t.Logf("[PASS] diarize → %d speakers, segments labeled", diar.NumSpeakers)

	// D) Subtitles from the diarized transcript → 2 artifacts.
	sjob := setup.PostJob(t, ctx, apiURL, key, audioArt, "export.subtitles", "1.0.0",
		fmt.Sprintf(`{"source_job_id":%q,"formats":["srt","vtt"]}`, djob))
	st, sres := setup.WaitForJobComplete(t, ctx, apiURL, key, sjob, setup.WaitJob)
	if st != "completed" {
		t.Fatalf("subtitles status=%s res=%s", st, sres)
	}
	var subs struct {
		Artifacts []struct {
			Format     string `json:"format"`
			ArtifactID string `json:"artifact_id"`
		} `json:"artifacts"`
	}
	_ = json.Unmarshal(sres, &subs)
	if len(subs.Artifacts) != 2 {
		t.Fatalf("subtitles produced %d artifacts: %s", len(subs.Artifacts), sres)
	}
	subArtIDs := []string{subs.Artifacts[0].ArtifactID, subs.Artifacts[1].ArtifactID}
	t.Logf("[PASS] subtitles → %d artifacts", len(subs.Artifacts))

	// E) Translate + F) Summarize (stub LLM).
	trjob := setup.PostJob(t, ctx, apiURL, key, audioArt, "text.translate", "1.0.0",
		fmt.Sprintf(`{"source_job_id":%q,"target_language":"es"}`, srcJob))
	st, trres := setup.WaitForJobComplete(t, ctx, apiURL, key, trjob, setup.WaitJob)
	if st != "completed" || !bytes.Contains(trres, []byte(`[es]`)) {
		t.Fatalf("translate status=%s res=%s", st, trres)
	}
	t.Logf("[PASS] translate → target es")

	sumjob := setup.PostJob(t, ctx, apiURL, key, audioArt, "text.summarize", "1.0.0",
		fmt.Sprintf(`{"source_job_id":%q,"mode":"bullets"}`, srcJob))
	st, sumres := setup.WaitForJobComplete(t, ctx, apiURL, key, sumjob, setup.WaitJob)
	if st != "completed" || !bytes.Contains(sumres, []byte(`"summary"`)) {
		t.Fatalf("summarize status=%s res=%s", st, sumres)
	}
	t.Logf("[PASS] summarize → bullets")

	// G) Content cache hit: re-submit the identical diarize job.
	code, body := post("/v1/jobs", fmt.Sprintf(
		`{"artifact_id":%q,"processor":{"name":"audio.diarize","version":"1.0.0"},"params":{"source_job_id":%q,"max_speakers":2}}`,
		audioArt, srcJob))
	if code != http.StatusOK {
		t.Fatalf("cache re-submit status=%d (want 200 hit), body=%s", code, body)
	}
	var cacheJob struct {
		Cache           string `json:"cache"`
		CachedFromJobID string `json:"cached_from_job_id"`
		Status          string `json:"status"`
	}
	_ = json.Unmarshal(body, &cacheJob)
	if cacheJob.Cache != "hit" || cacheJob.CachedFromJobID == "" || cacheJob.Status != "completed" {
		t.Fatalf("expected cache hit, got %s", body)
	}
	t.Logf("[PASS] cache hit → cached_from=%s", cacheJob.CachedFromJobID[:8])

	// H) Bundle the subtitle artifacts → worker zips → 302 download.
	code, body = post("/v1/bundles", fmt.Sprintf(
		`{"name":"e2e-subs","sources":[{"artifact_id":%q},{"artifact_id":%q}]}`, subArtIDs[0], subArtIDs[1]))
	if code != http.StatusAccepted {
		t.Fatalf("bundle create status=%d body=%s", code, body)
	}
	var bundle struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(body, &bundle)
	deadline := time.Now().Add(setup.WaitJob)
	var bstatus string
	for time.Now().Before(deadline) {
		_, gb := get("/v1/bundles/" + bundle.ID)
		var bv struct {
			Status string `json:"status"`
		}
		_ = json.Unmarshal(gb, &bv)
		bstatus = bv.Status
		if bstatus == "ready" || bstatus == "failed" {
			break
		}
		time.Sleep(setup.PollEvery)
	}
	if bstatus != "ready" {
		t.Fatalf("bundle did not become ready (last=%s)", bstatus)
	}
	// Download → 302 to a signed S3 URL.
	noRedirect := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	dreq, _ := http.NewRequestWithContext(ctx, http.MethodGet, apiURL+"/v1/bundles/"+bundle.ID+"/download", nil)
	dreq.Header.Set("X-API-Key", key)
	dresp, err := noRedirect.Do(dreq)
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	_ = dresp.Body.Close()
	if dresp.StatusCode != http.StatusFound || dresp.Header.Get("Location") == "" {
		t.Fatalf("bundle download = %d loc=%q, want 302", dresp.StatusCode, dresp.Header.Get("Location"))
	}
	t.Logf("[PASS] bundle ready + 302 download")

	// I) Webhook test-fire → recorded delivery attempt.
	code, body = post("/v1/webhooks", `{"url":"https://example.com/orpheus-e2e-hook","subscribed_events":["job.succeeded"]}`)
	if code != http.StatusCreated && code != http.StatusAccepted && code != http.StatusOK {
		t.Fatalf("webhook create status=%d body=%s", code, body)
	}
	var wh struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(body, &wh)
	code, body = post("/v1/webhooks/"+wh.ID+"/test", `{"event_type":"job.completed"}`)
	if code != http.StatusAccepted {
		t.Fatalf("test-fire status=%d body=%s", code, body)
	}
	var fired struct {
		DeliveryID string `json:"delivery_id"`
	}
	_ = json.Unmarshal(body, &fired)
	deadline = time.Now().Add(setup.WaitJob)
	var attempts int
	var sigBase string
	for time.Now().Before(deadline) {
		_, gb := get(fmt.Sprintf("/v1/webhooks/%s/deliveries/%s", wh.ID, fired.DeliveryID))
		var det struct {
			Attempts            []map[string]any `json:"attempts"`
			SignatureBaseString string           `json:"signature_base_string"`
			IsTest              bool             `json:"is_test"`
		}
		_ = json.Unmarshal(gb, &det)
		attempts = len(det.Attempts)
		sigBase = det.SignatureBaseString
		if attempts > 0 {
			if !det.IsTest {
				t.Fatalf("delivery not flagged is_test")
			}
			break
		}
		time.Sleep(setup.PollEvery)
	}
	if attempts == 0 || sigBase == "" {
		t.Fatalf("webhook test delivery had no recorded attempt / signature base (attempts=%d)", attempts)
	}
	t.Logf("[PASS] webhook test-fire → %d attempt(s), signature base recorded", attempts)

	t.Log("=== ALL PRD 01-05 E2E FEATURES PASSED THROUGH THE REAL PIPELINE ===")
}

// --- small HTTP + assertion helpers ----------------------------------------

func authReq(t *testing.T, ctx context.Context, method, url, key string, body []byte) (int, []byte) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		t.Fatalf("req: %v", err)
	}
	req.Header.Set("X-API-Key", key)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{Timeout: 8 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode, b
}

func assertHasKeys(t *testing.T, what string, raw []byte, keys ...string) {
	t.Helper()
	m := map[string]any{}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("%s: decode result: %v (%s)", what, err, raw)
	}
	for _, k := range keys {
		if _, ok := m[k]; !ok {
			t.Fatalf("%s: result missing key %q: %s", what, k, raw)
		}
	}
}

func trunc(b []byte) string {
	if len(b) > 120 {
		return string(b[:120]) + "…"
	}
	return string(b)
}
