// Phase 2 extract-metadata end-to-end smoke.
//
// Closes the loop the noop-job e2e (queue_e2e_test.go) leaves open:
// that test only proves the job row reaches the arq queue. This
// file proves the worker actually executes a real processor:
// downloads a WAV from MinIO, runs mutagen on it, and writes the
// result back into the jobs row.
//
// Flow:
//
//	seed org + api_key + processor + artifact
//	upload a 1s WAV to MinIO at artifact.s3_key
//	start the Go API in-process
//	start the Python arq worker subprocess (uses mutagen + boto3)
//	POST /v1/jobs {artifact_id, processor: extract-metadata}
//	  → outbox → NATS → arq → worker → jobs.status = 'completed'
//	poll GET /v1/jobs/{id} until status = 'completed'
//	assert result.duration_seconds ≈ 1.0 (±0.1)
//
// Gated on:
//
//	ORPHEUS_E2E=1
//	ORPHEUS_TEST_DATABASE_URL
//	ORPHEUS_TEST_REDIS_URL
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
// Hermetic on 127.0.0.1:18082 — distinct from TestE2E_PublicSurface
// (18080) and TestE2E_OutboxToWorkerQueue (18081).
package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/alexedwards/argon2id"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/redis/go-redis/v9"

	"github.com/orpheus/api/internal/audit"
	"github.com/orpheus/api/internal/auth"
	"github.com/orpheus/api/internal/config"
	"github.com/orpheus/api/internal/db"
	"github.com/orpheus/api/internal/handlers"
	"github.com/orpheus/api/internal/outbox"
	"github.com/orpheus/api/internal/queue"
	"github.com/orpheus/api/internal/server"
	"github.com/orpheus/api/internal/webhooks"
)

const (
	emTestProcessorName    = "extract-metadata"
	emTestProcessorVersion = "1.0.0"
	emTestAPIAddr          = "127.0.0.1:18082"
	emTestAPIURL           = "http://" + emTestAPIAddr
	emTestS3Key            = "e2e/extract-metadata/test.wav"
	emTestWait             = 15 * time.Second
	emTestPollEvery        = 500 * time.Millisecond
)

func TestE2E_ExtractMetadata(t *testing.T) {
	if os.Getenv("ORPHEUS_E2E") != "1" {
		t.Skip("set ORPHEUS_E2E=1 to run")
	}
	dsn := os.Getenv("ORPHEUS_TEST_DATABASE_URL")
	redisURL := os.Getenv("ORPHEUS_TEST_REDIS_URL")
	natsURL := os.Getenv("ORPHEUS_TEST_NATS_URL")
	s3Endpoint := os.Getenv("ORPHEUS_TEST_S3_ENDPOINT")
	s3Bucket := os.Getenv("ORPHEUS_TEST_S3_BUCKET")
	s3AccessKey := os.Getenv("ORPHEUS_TEST_S3_ACCESS_KEY")
	s3SecretKey := os.Getenv("ORPHEUS_TEST_S3_SECRET_KEY")
	if dsn == "" || redisURL == "" || natsURL == "" ||
		s3Endpoint == "" || s3Bucket == "" ||
		s3AccessKey == "" || s3SecretKey == "" {
		t.Skip("missing one of ORPHEUS_TEST_{DATABASE,REDIS,NATS,S3}_*_URL")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// ── 1. Postgres + migrations + service pool ───────────────────────
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	pingCtx, pingCancel := context.WithTimeout(ctx, 5*time.Second)
	defer pingCancel()
	if err := sqlDB.PingContext(pingCtx); err != nil {
		t.Fatalf("sql.PingContext: %v", err)
	}
	if err := db.Migrate(ctx, sqlDB); err != nil {
		t.Fatalf("db.Migrate: %v", err)
	}

	pool, err := emNewServicePool(ctx, dsn)
	if err != nil {
		t.Fatalf("db.New (service pool): %v", err)
	}
	t.Cleanup(pool.Close)

	// ── 2. NATS + Redis clients ───────────────────────────────────────
	natsConn, err := nats.Connect(natsURL, nats.Name("orpheus-api-extract-metadata-e2e"))
	if err != nil {
		t.Fatalf("nats.Connect: %v", err)
	}
	t.Cleanup(func() { natsConn.Close() })

	rdb, err := emOpenRedis(redisURL)
	if err != nil {
		t.Fatalf("openRedis: %v", err)
	}
	t.Cleanup(func() { _ = rdb.Close() })

	// ── 3. Background workers: outbox publisher + arq enqueuer ────────
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	js, err := jetstream.New(natsConn)
	if err != nil {
		t.Fatalf("jetstream.New: %v", err)
	}
	publisher := outbox.New(pool, js, logger)
	arqEnq := queue.NewArqEnqueuer(natsConn, rdb, logger)
	delivery := webhooks.New(pool, logger, natsConn, nil)

	bgCtx, bgCancel := context.WithCancel(ctx)
	defer bgCancel()

	var workers sync.WaitGroup
	workers.Add(1)
	go func() {
		defer workers.Done()
		if err := publisher.Run(bgCtx); err != nil {
			t.Logf("outbox.publisher exited: %v", err)
		}
	}()
	workers.Add(1)
	go func() {
		defer workers.Done()
		if err := arqEnq.Run(bgCtx); err != nil {
			t.Logf("queue.arq_enqueuer exited: %v", err)
		}
	}()
	workers.Add(1)
	go func() {
		defer workers.Done()
		if err := delivery.Run(bgCtx); err != nil {
			t.Logf("webhooks.delivery exited: %v", err)
		}
	}()

	// ── 4. API server with /v1 routes mounted ─────────────────────────
	apikey := auth.NewAPIKeyValidator(pool)
	authn := &auth.Authenticator{APIKey: apikey}
	auditRec := audit.New(pool, logger)
	cfg := &config.Config{
		Env:                  "test",
		Host:                 "127.0.0.1",
		Port:                 18082,
		ShutdownGraceSeconds: 5,
	}
	srv := server.NewWithOptions(cfg, logger, server.Options{
		DB:    pool,
		Authn: authn,
		Audit: auditRec,
	})

	errCh := make(chan error, 1)
	go func() {
		if err := srv.Run(bgCtx); err != nil {
			errCh <- err
		}
		close(errCh)
	}()
	emWaitForListener(t, emTestAPIAddr, 5*time.Second)
	t.Cleanup(func() {
		bgCancel()
		select {
		case <-errCh:
		case <-time.After(5 * time.Second):
			t.Logf("server did not exit within shutdown grace")
		}
		workers.Wait()
	})

	// ── 5. Seed org → user → api key → processor → artifact ──────────
	orgID := uuid.NewString()
	userID := uuid.NewString()
	processorID := uuid.NewString()
	processorVersionID := uuid.NewString()
	artifactID := uuid.NewString()

	emSeedOrg(t, pool, orgID)
	emSeedUser(t, pool, orgID, userID)
	apiKey := emSeedAPIKey(t, pool, orgID)
	emSeedProcessor(t, pool, processorID, processorVersionID)

	t.Cleanup(func() {
		cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanCancel()
		_, _ = pool.Exec(cleanCtx, `DELETE FROM outbox WHERE org_id = $1`, orgID)
		_, _ = pool.Exec(cleanCtx, `DELETE FROM jobs WHERE org_id = $1`, orgID)
		_, _ = pool.Exec(cleanCtx, `DELETE FROM api_keys WHERE org_id = $1`, orgID)
		_, _ = pool.Exec(cleanCtx, `DELETE FROM audit_log WHERE org_id = $1`, orgID)
		_, _ = pool.Exec(cleanCtx, `DELETE FROM artifacts WHERE id = $1`, artifactID)
		_, _ = pool.Exec(cleanCtx, `DELETE FROM processor_versions WHERE id = $1`, processorVersionID)
		_, _ = pool.Exec(cleanCtx, `DELETE FROM processors WHERE id = $1`, processorID)
		_, _ = pool.Exec(cleanCtx, `DELETE FROM users WHERE id = $1`, userID)
		_, _ = pool.Exec(cleanCtx, `DELETE FROM organizations WHERE id = $1`, orgID)
	})

	// ── 6. Upload a 1s WAV to MinIO at the artifact's key ─────────────
	wav := buildWAV(1*time.Second, 8000, 1)
	emEnsureBucket(t, ctx, s3Endpoint, s3Bucket, s3AccessKey, s3SecretKey)
	emUploadS3(t, ctx, s3Endpoint, s3Bucket, emTestS3Key, wav, s3AccessKey, s3SecretKey)
	t.Cleanup(func() {
		emDeleteS3(t, ctx, s3Endpoint, s3Bucket, emTestS3Key, s3AccessKey, s3SecretKey)
	})

	emSeedArtifact(t, pool, orgID, artifactID, s3Bucket, emTestS3Key, len(wav))

	// ── 7. Start the Python Arq worker subprocess ─────────────────────
	workerCmd := exec.CommandContext(bgCtx,
		"uv", "run", "--package", "orpheus-workers",
		"python", "-m", "orpheus_workers.worker",
	)
	workerCmd.Dir = emFindWorkspaceRoot(t)
	workerCmd.Env = append(os.Environ(),
		"ORPHEUS_WORKER_REDIS_URL="+redisURL,
		"ORPHEUS_WORKER_DATABASE_URL="+dsn,
		"ORPHEUS_WORKER_S3_ENDPOINT="+s3Endpoint,
		"ORPHEUS_WORKER_S3_BUCKET="+s3Bucket,
		"ORPHEUS_WORKER_S3_ACCESS_KEY="+s3AccessKey,
		"ORPHEUS_WORKER_S3_SECRET_KEY="+s3SecretKey,
	)
	workerCmd.Stdout = os.Stdout
	workerCmd.Stderr = os.Stderr
	if err := workerCmd.Start(); err != nil {
		t.Fatalf("worker Start: %v", err)
	}
	t.Cleanup(func() {
		if workerCmd.Process != nil {
			_ = workerCmd.Process.Kill()
			_, _ = workerCmd.Process.Wait()
		}
	})

	// The worker has more cold-start work than the noop e2e: `uv run`
	// spins up the venv, boto3 loads, mutagen loads, then arq connects
	// to Redis. Two seconds is comfortable on a warm machine; CI isn't
	// running this test anyway.
	time.Sleep(2 * time.Second)

	// ── 8. POST /v1/jobs ─────────────────────────────────────────────
	body, _ := json.Marshal(handlers.CreateJobRequest{
		ArtifactID: artifactID,
		Processor:  handlers.ProcessorRef{Name: emTestProcessorName, Version: emTestProcessorVersion},
	})
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, emTestAPIURL+"/v1/jobs", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-API-Key", apiKey)

	httpClient := &http.Client{Timeout: 5 * time.Second}
	resp, err := httpClient.Do(httpReq)
	if err != nil {
		t.Fatalf("POST /v1/jobs: %v", err)
	}
	respBody, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST /v1/jobs status = %d, want 202; body=%s", resp.StatusCode, respBody)
	}
	var job handlers.Job
	if err := json.Unmarshal(respBody, &job); err != nil {
		t.Fatalf("decode job: %v (body=%s)", err, respBody)
	}
	if job.ID == "" {
		t.Fatal("job.id is empty")
	}

	// ── 9. Poll GET /v1/jobs/{id} until status=completed ─────────────
	deadline := time.Now().Add(emTestWait)
	var final handlers.Job
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, emTestAPIURL+"/v1/jobs/"+job.ID, nil)
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		req.Header.Set("X-API-Key", apiKey)
		r, err := httpClient.Do(req)
		if err != nil {
			t.Fatalf("GET /v1/jobs/%s: %v", job.ID, err)
		}
		body, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()
		if r.StatusCode != http.StatusOK {
			t.Fatalf("GET /v1/jobs/%s status = %d, want 200; body=%s", job.ID, r.StatusCode, body)
		}
		if err := json.Unmarshal(body, &final); err != nil {
			t.Fatalf("decode job: %v (body=%s)", err, body)
		}
		if final.Status == "completed" || final.Status == "failed" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %s did not reach terminal status within %s; last status = %s",
				job.ID, emTestWait, final.Status)
		}
		time.Sleep(emTestPollEvery)
	}

	if final.Status != "completed" {
		t.Fatalf("job %s status = %s, want completed; result = %s", job.ID, final.Status, string(final.Result))
	}

	// ── 10. Assert the result payload ────────────────────────────────
	var result map[string]any
	if err := json.Unmarshal(final.Result, &result); err != nil {
		t.Fatalf("decode result: %v (raw=%s)", err, string(final.Result))
	}
	durRaw, ok := result["duration_seconds"]
	if !ok {
		t.Fatalf("result has no duration_seconds key: %s", string(final.Result))
	}
	dur, ok := durRaw.(float64)
	if !ok {
		t.Fatalf("duration_seconds is %T, want float64", durRaw)
	}
	if math.IsNaN(dur) || dur < 0.9 || dur > 1.1 {
		t.Fatalf("duration_seconds = %v, want value in (0.9, 1.1); full result = %s", dur, string(final.Result))
	}
}

// ─────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────

// buildWAV synthesises a minimal RIFF/WAVE file: 1 fmt chunk, 1 data
// chunk of N samples of 16-bit mono silence. mutagen only needs the
// header to be valid; the body is irrelevant for extracting length
// and sample rate.
func buildWAV(d time.Duration, sampleRate, channels int) []byte {
	samples := int(d.Seconds() * float64(sampleRate))
	buf := &bytes.Buffer{}
	w := func(v any) { _ = binary.Write(buf, binary.LittleEndian, v) }
	w([]byte("RIFF"))
	w(uint32(36 + samples*channels*2))
	w([]byte("WAVE"))
	w([]byte("fmt "))
	w(uint32(16))
	w(uint16(1))
	w(uint16(channels))
	w(uint32(sampleRate))
	w(uint32(sampleRate * channels * 2))
	w(uint16(channels * 2))
	w(uint16(16))
	w([]byte("data"))
	w(uint32(samples * channels * 2))
	silence := make([]int16, samples*channels)
	w(silence)
	return buf.Bytes()
}

// newS3Client builds an S3 client targeting a custom MinIO endpoint
// with path-style addressing. Mirrors the construction in
// apps/api/internal/storage/s3/client.go.
func newS3Client(ctx context.Context, endpoint, accessKey, secretKey string) (*awss3.Client, error) {
	creds := credentials.NewStaticCredentialsProvider(accessKey, secretKey, "")
	awsCfg, err := awsconfig.LoadDefaultConfig(
		ctx,
		awsconfig.WithRegion("us-east-1"),
		awsconfig.WithCredentialsProvider(creds),
	)
	if err != nil {
		return nil, fmt.Errorf("aws.load_config: %w", err)
	}
	return awss3.NewFromConfig(awsCfg, func(o *awss3.Options) {
		o.BaseEndpoint = aws.String(endpoint)
		o.UsePathStyle = true
	}), nil
}

func emUploadS3(t *testing.T, ctx context.Context, endpoint, bucket, key string, body []byte, accessKey, secretKey string) {
	t.Helper()
	client, err := newS3Client(ctx, endpoint, accessKey, secretKey)
	if err != nil {
		t.Fatalf("newS3Client: %v", err)
	}
	if _, err := client.PutObject(ctx, &awss3.PutObjectInput{
		Bucket:      aws.String(bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(body),
		ContentType: aws.String("audio/wav"),
	}); err != nil {
		t.Fatalf("s3.PutObject(%s/%s): %v", bucket, key, err)
	}
}

func emEnsureBucket(t *testing.T, ctx context.Context, endpoint, bucket, accessKey, secretKey string) {
	t.Helper()
	client, err := newS3Client(ctx, endpoint, accessKey, secretKey)
	if err != nil {
		t.Fatalf("newS3Client (ensure bucket): %v", err)
	}
	_, err = client.HeadBucket(ctx, &awss3.HeadBucketInput{Bucket: aws.String(bucket)})
	if err == nil {
		return
	}
	if _, err := client.CreateBucket(ctx, &awss3.CreateBucketInput{Bucket: aws.String(bucket)}); err != nil {
		t.Fatalf("s3.CreateBucket(%s): %v", bucket, err)
	}
}

func emDeleteS3(t *testing.T, ctx context.Context, endpoint, bucket, key, accessKey, secretKey string) {
	t.Helper()
	client, err := newS3Client(ctx, endpoint, accessKey, secretKey)
	if err != nil {
		t.Logf("newS3Client (delete): %v", err)
		return
	}
	_, err = client.DeleteObject(ctx, &awss3.DeleteObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Logf("s3.DeleteObject(%s/%s): %v", bucket, key, err)
	}
}

func emSeedUser(t *testing.T, pool *db.DB, orgID, userID string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO users (id, org_id, email, name) VALUES ($1, $2, $3, $4)`,
		userID, orgID, "extract-metadata-e2e@example.com", "extract-metadata-e2e",
	); err != nil {
		t.Fatalf("insert user: %v", err)
	}
}

func emSeedOrg(t *testing.T, pool *db.DB, orgID string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO organizations (id, name, slug) VALUES ($1, $2, $3)`,
		orgID, "extract-metadata-e2e", "extract-metadata-e2e-"+orgID[:8],
	); err != nil {
		t.Fatalf("insert org: %v", err)
	}
}

func emSeedAPIKey(t *testing.T, pool *db.DB, orgID string) string {
	t.Helper()
	body := make([]byte, 32)
	if _, err := rand.Read(body); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	secret := "ak_live_" + base64.RawURLEncoding.EncodeToString(body)
	prefix := secret[:9]
	hashed, err := argon2id.CreateHash(secret, argon2id.DefaultParams)
	if err != nil {
		t.Fatalf("argon2id.CreateHash: %v", err)
	}
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO api_keys (id, org_id, name, hashed_secret, prefix, scopes) VALUES ($1, $2, $3, $4, $5, $6)`,
		uuid.NewString(), orgID, "extract-metadata-e2e", hashed, prefix,
		[]string{"jobs:write", "jobs:read"},
	); err != nil {
		t.Fatalf("insert api_key: %v", err)
	}
	return secret
}

func emSeedProcessor(t *testing.T, pool *db.DB, processorID, versionID string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO processors (id, name, display_name, tier, timeout_seconds) VALUES ($1, $2, $3, 'cpu_tiny', 60)`,
		processorID, emTestProcessorName, "Extract Metadata",
	); err != nil {
		t.Fatalf("insert processor: %v", err)
	}
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO processor_versions (id, processor_id, version, model_id, model_version_id) VALUES ($1, $2, $3, $4, $5)`,
		versionID, processorID, emTestProcessorVersion, "model-extract-metadata", "v1",
	); err != nil {
		t.Fatalf("insert processor_version: %v", err)
	}
}

func emSeedArtifact(t *testing.T, pool *db.DB, orgID, artifactID, bucket, key string, size int) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO artifacts (id, org_id, s3_bucket, s3_key, sha256, size_bytes, content_type)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		artifactID, orgID, bucket, key,
		"e2e-extract-metadata-sha256", size, "audio/wav",
	); err != nil {
		t.Fatalf("insert artifact: %v", err)
	}
}

// emWaitForListener polls a TCP address until the listener accepts or
// the deadline elapses. Renamed from queue_e2e_test.go's waitForListener
// (the hard rule forbids editing that file to share the helper).
func emWaitForListener(t *testing.T, addr string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		c, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("listener %s did not become reachable within %s", addr, timeout)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func emOpenRedis(rawURL string) (*redis.Client, error) {
	opts, err := redis.ParseURL(rawURL)
	if err != nil {
		return nil, fmt.Errorf("redis.parse: %w", err)
	}
	return redis.NewClient(opts), nil
}

func emFindWorkspaceRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "pyproject.toml")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("could not find workspace root (pyproject.toml) from %s", dir)
		}
		dir = parent
	}
}

func emNewServicePool(ctx context.Context, dsn string) (*db.DB, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("db.parse_dsn: %w", err)
	}
	cfg.MaxConns = 20
	cfg.MinConns = 2
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SET app.is_service = 'true'")
		return err
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("db.connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("db.ping: %w", err)
	}
	return &db.DB{Pool: pool}, nil
}
