// Package setup holds the shared e2e test setup code for the
// processor smokes (extract-metadata, probe, ...). Test files in
// apps/api/internal/e2e/ import this package and call its helpers.
//
// The package is a regular Go package (not a _test.go file) so
// other test packages can import it. It is small and only does
// work when called, so the production-binary footprint is
// negligible.
package setup

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/alexedwards/argon2id"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/orpheus/api/internal/audit"
	"github.com/orpheus/api/internal/auth"
	"github.com/orpheus/api/internal/config"
	"github.com/orpheus/api/internal/db"
	"github.com/orpheus/api/internal/metrics"
	"github.com/orpheus/api/internal/outbox"
	"github.com/orpheus/api/internal/server"
	"github.com/orpheus/api/internal/storage/s3"
	"github.com/orpheus/api/internal/webhooks"
)

const (
	APIPortExtractMetadata = 18082
	APIPortProbe           = 18083
	WaitJob                = 15 * time.Second
	PollEvery              = 500 * time.Millisecond
)

// ── Postgres ─────────────────────────────────────────────────────────

// ServicePool opens a pgx pool that bypasses RLS by setting
// app.is_service = 'true' on every connection. The pool is closed
// automatically at end-of-test.
func ServicePool(t *testing.T, ctx context.Context, dsn string) *db.DB {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("db.parse_dsn: %v", err)
	}
	cfg.MaxConns = 20
	cfg.MinConns = 2
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SET app.is_service = 'true'")
		return err
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("db.connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("db.ping: %v", err)
	}
	return &db.DB{Pool: pool}
}

// RunMigrations opens a *sql.DB, pings, runs db.Migrate, and
// registers sqlDB.Close for end-of-test.
func RunMigrations(t *testing.T, ctx context.Context, dsn string) *sql.DB {
	t.Helper()
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := sqlDB.PingContext(pingCtx); err != nil {
		t.Fatalf("sql.PingContext: %v", err)
	}
	if err := db.Migrate(ctx, sqlDB); err != nil {
		t.Fatalf("db.Migrate: %v", err)
	}
	return sqlDB
}

// ── Seeders ──────────────────────────────────────────────────────────

// SeedOrg inserts a fresh organizations row.
func SeedOrg(t *testing.T, ctx context.Context, pool *db.DB, orgID string) {
	t.Helper()
	if _, err := pool.Exec(ctx,
		`INSERT INTO organizations (id, name, slug) VALUES ($1, $2, $3)`,
		orgID, "e2e-"+orgID[:8], "e2e-"+orgID[:8],
	); err != nil {
		t.Fatalf("insert org: %v", err)
	}
}

// SeedUser inserts a fresh users row and returns its id.
func SeedUser(t *testing.T, ctx context.Context, pool *db.DB, orgID string) string {
	t.Helper()
	userID := uuid.NewString()
	if _, err := pool.Exec(ctx,
		`INSERT INTO users (id, org_id, email, name) VALUES ($1, $2, $3, $4)`,
		userID, orgID, "e2e-"+userID[:8]+"@example.com", "e2e-user",
	); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	return userID
}

// SeedAPIKey generates `ak_live_<base64>`, Argon2id-hashes it,
// inserts the api_keys row, and returns the cleartext secret.
func SeedAPIKey(t *testing.T, ctx context.Context, pool *db.DB, orgID string) string {
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
	if _, err := pool.Exec(ctx,
		`INSERT INTO api_keys (id, org_id, name, hashed_secret, prefix, scopes) VALUES ($1, $2, $3, $4, $5, $6)`,
		uuid.NewString(), orgID, "e2e", hashed, prefix,
		[]string{"jobs:write", "jobs:read"},
	); err != nil {
		t.Fatalf("insert api_key: %v", err)
	}
	return secret
}

// SeedProcessor inserts a (processors, processor_versions) pair
// and returns the two ids.
func SeedProcessor(t *testing.T, ctx context.Context, pool *db.DB, name, version string) (processorID, versionID string) {
	t.Helper()
	processorID = uuid.NewString()
	versionID = uuid.NewString()
	if _, err := pool.Exec(ctx,
		`INSERT INTO processors (id, name, display_name, tier, timeout_seconds) VALUES ($1, $2, $3, 'cpu_tiny', 60)`,
		processorID, name, name,
	); err != nil {
		t.Fatalf("insert processor: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO processor_versions (id, processor_id, version, model_id, model_version_id) VALUES ($1, $2, $3, $4, $5)`,
		versionID, processorID, version, "model-"+name, "v1",
	); err != nil {
		t.Fatalf("insert processor_version: %v", err)
	}
	return processorID, versionID
}

// SeedArtifact inserts a fresh artifacts row pointing at the
// given S3 object and returns the id.
func SeedArtifact(t *testing.T, ctx context.Context, pool *db.DB, orgID, bucket, key string, size int) string {
	t.Helper()
	artifactID := uuid.NewString()
	if _, err := pool.Exec(ctx,
		`INSERT INTO artifacts (id, org_id, s3_bucket, s3_key, sha256, size_bytes, content_type)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		artifactID, orgID, bucket, key, "e2e-sha256", size, "audio/wav",
	); err != nil {
		t.Fatalf("insert artifact: %v", err)
	}
	return artifactID
}

// CleanupOrgData registers a t.Cleanup that wipes per-org data
// the test wrote. Processors and artifacts are NOT touched — the
// test owns their lifecycle.
func CleanupOrgData(t *testing.T, pool *db.DB, orgID string) {
	t.Helper()
	t.Cleanup(func() {
		cleanCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = pool.Exec(cleanCtx, `DELETE FROM outbox WHERE org_id = $1`, orgID)
		_, _ = pool.Exec(cleanCtx, `DELETE FROM jobs WHERE org_id = $1`, orgID)
		_, _ = pool.Exec(cleanCtx, `DELETE FROM api_keys WHERE org_id = $1`, orgID)
		_, _ = pool.Exec(cleanCtx, `DELETE FROM audit_log WHERE org_id = $1`, orgID)
	})
}

// ── WAV fixture ──────────────────────────────────────────────────────

// BuildWAV synthesises a minimal RIFF/WAVE file: 1 fmt chunk, 1
// data chunk of N samples of 16-bit silence. ffprobe only needs
// the header to be valid; the body is irrelevant for extracting
// length, sample rate, channels, and codec.
func BuildWAV(seconds float64, sampleRate, channels int) []byte {
	samples := int(seconds * float64(sampleRate))
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

// ── S3 ───────────────────────────────────────────────────────────────

// newS3Client builds an S3 client targeting a custom MinIO
// endpoint with path-style addressing. Mirrors apps/api/internal/
// storage/s3/client.go.
func newS3Client(t *testing.T, ctx context.Context, endpoint, accessKey, secretKey string) *awss3.Client {
	t.Helper()
	creds := credentials.NewStaticCredentialsProvider(accessKey, secretKey, "")
	awsCfg, err := awsconfig.LoadDefaultConfig(
		ctx,
		awsconfig.WithRegion("us-east-1"),
		awsconfig.WithCredentialsProvider(creds),
	)
	if err != nil {
		t.Fatalf("aws.load_config: %v", err)
	}
	return awss3.NewFromConfig(awsCfg, func(o *awss3.Options) {
		o.BaseEndpoint = aws.String(endpoint)
		o.UsePathStyle = true
	})
}

// UploadS3 PUTs body to the given S3 key.
func UploadS3(t *testing.T, ctx context.Context, endpoint, bucket, key string, body []byte, accessKey, secretKey string) {
	t.Helper()
	client := newS3Client(t, ctx, endpoint, accessKey, secretKey)
	if _, err := client.PutObject(ctx, &awss3.PutObjectInput{
		Bucket:      aws.String(bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(body),
		ContentType: aws.String("audio/wav"),
	}); err != nil {
		t.Fatalf("s3.PutObject(%s/%s): %v", bucket, key, err)
	}
}

// EnsureBucket creates the bucket if it does not exist.
func EnsureBucket(t *testing.T, ctx context.Context, endpoint, bucket, accessKey, secretKey string) {
	t.Helper()
	client := newS3Client(t, ctx, endpoint, accessKey, secretKey)
	_, err := client.HeadBucket(ctx, &awss3.HeadBucketInput{Bucket: aws.String(bucket)})
	if err == nil {
		return
	}
	if _, err := client.CreateBucket(ctx, &awss3.CreateBucketInput{Bucket: aws.String(bucket)}); err != nil {
		// Idempotent: a bucket we already own is success. (The first S3 call
		// on a fresh client can also fail HeadBucket above under request
		// clock skew — the SDK self-corrects on the retry, so CreateBucket
		// then reports the bucket already exists rather than creating it.)
		var owned *s3types.BucketAlreadyOwnedByYou
		var exists *s3types.BucketAlreadyExists
		if errors.As(err, &owned) || errors.As(err, &exists) {
			return
		}
		t.Fatalf("s3.CreateBucket(%s): %v", bucket, err)
	}
}

// DeleteS3 removes the S3 object (best-effort).
func DeleteS3(t *testing.T, ctx context.Context, endpoint, bucket, key, accessKey, secretKey string) {
	t.Helper()
	client := newS3Client(t, ctx, endpoint, accessKey, secretKey)
	if _, err := client.DeleteObject(ctx, &awss3.DeleteObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	}); err != nil {
		t.Logf("s3.DeleteObject(%s/%s): %v", bucket, key, err)
	}
}

// ── Workspace + listener ─────────────────────────────────────────────

// FindWorkspaceRoot walks up from cwd until it finds a directory
// containing pyproject.toml — the monorepo root that `uv run
// --package orpheus-workers` operates from.
func FindWorkspaceRoot(t *testing.T) string {
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

// WaitForListener polls a TCP address until the listener accepts
// or the deadline elapses.
func WaitForListener(t *testing.T, addr string, timeout time.Duration) {
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

// ── API + worker lifecycle ───────────────────────────────────────────

// StartAPI boots an in-process API server (full /v1 surface) on
// the given port, plus the outbox publisher + webhooks delivery
// background loops. Returns the base URL and a shutdown func that
// cancels the background context and waits for the loops to exit.
func StartAPI(t *testing.T, ctx context.Context, pool *db.DB, natsURL string, port int) (string, func()) {
	t.Helper()

	natsConn, err := nats.Connect(natsURL, nats.Name(fmt.Sprintf("orpheus-api-e2e-%d", port)))
	if err != nil {
		t.Fatalf("nats.Connect: %v", err)
	}
	t.Cleanup(func() { natsConn.Close() })

	js, err := jetstream.New(natsConn)
	if err != nil {
		t.Fatalf("jetstream.New: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mtr := metrics.New()
	publisher := outbox.New(pool, js, mtr, logger)
	delivery := webhooks.New(pool, logger, natsConn, nil)

	bgCtx, bgCancel := context.WithCancel(ctx)

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
		if err := delivery.Run(bgCtx); err != nil {
			t.Logf("webhooks.delivery exited: %v", err)
		}
	}()

	apikey := auth.NewAPIKeyValidator(pool)
	authn := &auth.Authenticator{APIKey: apikey}
	auditRec := audit.New(pool, logger)
	cfg := &config.Config{
		Env:                  "test",
		Host:                 "127.0.0.1",
		Port:                 port,
		ShutdownGraceSeconds: 5,
	}

	// Wire an S3 client from the test env when present so S3-backed
	// endpoints (artifact signed-url, bundle download) are exercisable in
	// e2e. Absent env → nil, and those endpoints simply aren't driven.
	var s3c *s3.Client
	if ep := os.Getenv("ORPHEUS_TEST_S3_ENDPOINT"); ep != "" {
		if c, err := s3.New(bgCtx, &config.Config{
			S3Endpoint:  ep,
			S3AccessKey: os.Getenv("ORPHEUS_TEST_S3_ACCESS_KEY"),
			S3SecretKey: os.Getenv("ORPHEUS_TEST_S3_SECRET_KEY"),
			S3Bucket:    os.Getenv("ORPHEUS_TEST_S3_BUCKET"),
		}); err == nil {
			s3c = c
		} else {
			t.Logf("e2e: s3 client unavailable: %v", err)
		}
	}

	srv := server.NewWithOptions(cfg, logger, server.Options{
		DB:      pool,
		S3:      s3c,
		Authn:   authn,
		Audit:   auditRec,
		Metrics: mtr,
	})

	errCh := make(chan error, 1)
	go func() {
		if err := srv.Run(bgCtx); err != nil {
			errCh <- err
		}
		close(errCh)
	}()

	addr := cfg.Addr()
	WaitForListener(t, addr, 5*time.Second)

	shutdown := func() {
		bgCancel()
		select {
		case <-errCh:
		case <-time.After(5 * time.Second):
			t.Logf("server did not exit within shutdown grace")
		}
		workers.Wait()
	}
	return "http://" + addr, shutdown
}

// StartWorker spawns the Python JetStream worker subprocess. The
// subprocess inherits the parent env plus ORPHEUS_WORKER_{NATS_URL,
// DATABASE_URL, S3_*} so it can run without the operator exporting
// them. The subprocess is put in its own process group and the
// whole group is killed at end-of-test — `uv run` spawns the
// Python process as a child, and killing the uv process alone
// would orphan the worker. stdout/stderr go to a buffer so a slow
// drain does not block the test from exiting.
func StartWorker(t *testing.T, ctx context.Context, natsURL, dsn, s3Endpoint, s3Bucket, s3AccessKey, s3SecretKey string) {
	t.Helper()

	bgCtx, bgCancel := context.WithCancel(ctx)
	workerCmd := exec.CommandContext(bgCtx,
		"uv", "run", "--package", "orpheus-workers",
		"python", "-m", "orpheus_workers.worker",
	)
	workerCmd.Dir = FindWorkspaceRoot(t)
	workerCmd.Env = append(os.Environ(),
		"ORPHEUS_WORKER_NATS_URL="+natsURL,
		"ORPHEUS_WORKER_DATABASE_URL="+dsn,
		"ORPHEUS_WORKER_S3_ENDPOINT="+s3Endpoint,
		"ORPHEUS_WORKER_S3_BUCKET="+s3Bucket,
		"ORPHEUS_WORKER_S3_ACCESS_KEY="+s3AccessKey,
		"ORPHEUS_WORKER_S3_SECRET_KEY="+s3SecretKey,
	)
	workerCmd.Stdout = io.Discard
	workerCmd.Stderr = io.Discard
	workerCmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := workerCmd.Start(); err != nil {
		bgCancel()
		t.Fatalf("worker Start: %v", err)
	}
	// The worker has more cold-start work than the noop e2e: `uv
	// run` spins up the venv, boto3 loads, ffprobe loads, then the
	// worker subscribes to JetStream. Two seconds is comfortable
	// on a warm machine (the e2e CI job caches the whisper model so a
	// cold first run isn't paying a download here).
	time.Sleep(2 * time.Second)

	t.Cleanup(func() {
		if workerCmd.Process != nil {
			_ = syscall.Kill(-workerCmd.Process.Pid, syscall.SIGKILL)
			_, _ = workerCmd.Process.Wait()
		}
		bgCancel()
	})
}

// ── HTTP helpers ─────────────────────────────────────────────────────

// PostJob POSTs {artifact_id, processor:{name,version}, params?}
// to /v1/jobs and returns the new job id. params is the raw JSON
// blob stored in jobs.params (e.g. `{"start_seconds":0.5}`);
// pass "" when the processor takes no params.
func PostJob(t *testing.T, ctx context.Context, apiURL, secret, artifactID, procName, procVersion, params string) string {
	t.Helper()
	type procRef struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	body, err := json.Marshal(struct {
		ArtifactID string          `json:"artifact_id"`
		Processor  procRef         `json:"processor"`
		Params     json.RawMessage `json:"params,omitempty"`
	}{
		ArtifactID: artifactID,
		Processor:  procRef{Name: procName, Version: procVersion},
		Params:     json.RawMessage(params),
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL+"/v1/jobs", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-API-Key", secret)
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		t.Fatalf("POST /v1/jobs: %v", err)
	}
	respBody, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST /v1/jobs status = %d, want 202; body=%s", resp.StatusCode, respBody)
	}
	var job struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(respBody, &job); err != nil {
		t.Fatalf("decode job: %v (body=%s)", err, respBody)
	}
	if job.ID == "" {
		t.Fatal("job.id is empty")
	}
	return job.ID
}

// WaitForJobComplete polls GET /v1/jobs/{id} until status
// reaches a terminal state. Returns the final job status and
// result payload (raw json).
func WaitForJobComplete(t *testing.T, ctx context.Context, apiURL, secret, jobID string, wait time.Duration) (status string, result []byte) {
	t.Helper()
	client := &http.Client{Timeout: 5 * time.Second}
	deadline := time.Now().Add(wait)
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL+"/v1/jobs/"+jobID, nil)
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		req.Header.Set("X-API-Key", secret)
		r, err := client.Do(req)
		if err != nil {
			t.Fatalf("GET /v1/jobs/%s: %v", jobID, err)
		}
		body, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()
		if r.StatusCode != http.StatusOK {
			t.Fatalf("GET /v1/jobs/%s status = %d, want 200; body=%s", jobID, r.StatusCode, body)
		}
		var job struct {
			Status string          `json:"status"`
			Result json.RawMessage `json:"result"`
		}
		if err := json.Unmarshal(body, &job); err != nil {
			t.Fatalf("decode job: %v (body=%s)", err, body)
		}
		if job.Status == "completed" || job.Status == "failed" {
			return job.Status, job.Result
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %s did not reach terminal status within %s; last status = %s",
				jobID, wait, job.Status)
		}
		time.Sleep(PollEvery)
	}
}
