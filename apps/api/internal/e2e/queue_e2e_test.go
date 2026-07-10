// Phase 2 queue end-to-end smoke.
//
// Exercises the full outbox → NATS → Arq → Python worker loop:
//
//	POST /v1/jobs
//	  → jobs row + outbox row in the same db tx
//	  → outbox.Publisher drains the outbox to NATS adkil.job.queued
//	  → queue.ArqEnqueuer subscribes and LPUSHes an arq job to the
//	    Redis key "arq:result:queue"
//	  → the Python arq worker subprocess (orpheus_workers.worker)
//	    LPOPs the job and runs noop_job
//
// The test polls LLEN("arq:result:queue") for zero — option A from
// the brief. It is gated on four env vars so CI without the local
// docker stack stays green:
//
//	ORPHEUS_E2E=1
//	ORPHEUS_TEST_DATABASE_URL
//	ORPHEUS_TEST_REDIS_URL
//	ORPHEUS_TEST_NATS_URL
//
// The test is hermetic on its own port (18081) so it does not
// collide with TestE2E_PublicSurface in e2e_test.go.
package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/alexedwards/argon2id"
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
	queueTestProcessorName    = "extract-metadata"
	queueTestProcessorVersion = "1.0.0"

	queueTestArqQueueKey = "arq:result:queue"
	queueTestPollEvery   = 200 * time.Millisecond
	queueTestWait        = 10 * time.Second
	queueTestAPIAddr     = "127.0.0.1:18081"
	queueTestAPIURL      = "http://" + queueTestAPIAddr
)

// TestE2E_OutboxToWorkerQueue boots Postgres / Redis / NATS, starts
// the Go API in-process with the outbox publisher + arq enqueuer
// wired as goroutines, spawns the Python arq worker as a subprocess,
// POSTs a job through the public surface, and asserts the arq queue
// is drained within the budget.
func TestE2E_OutboxToWorkerQueue(t *testing.T) {
	if os.Getenv("ORPHEUS_E2E") != "1" {
		t.Skip("set ORPHEUS_E2E=1 to run")
	}
	dsn := os.Getenv("ORPHEUS_TEST_DATABASE_URL")
	redisURL := os.Getenv("ORPHEUS_TEST_REDIS_URL")
	natsURL := os.Getenv("ORPHEUS_TEST_NATS_URL")
	if dsn == "" || redisURL == "" || natsURL == "" {
		t.Skip("ORPHEUS_TEST_{DATABASE,REDIS,NATS}_URL not set; cannot run queue e2e")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// ── 1. Postgres + migrations ──────────────────────────────────────
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

	pool, err := newServicePool(ctx, dsn)
	if err != nil {
		t.Fatalf("db.New (service pool): %v", err)
	}
	t.Cleanup(pool.Close)

	// ── 2. NATS + Redis clients ───────────────────────────────────────
	natsConn, err := nats.Connect(natsURL, nats.Name("orpheus-api-e2e"))
	if err != nil {
		t.Fatalf("nats.Connect: %v", err)
	}
	t.Cleanup(func() { natsConn.Close() })

	rdb, err := openRedis(redisURL)
	if err != nil {
		t.Fatalf("openRedis (enqueuer): %v", err)
	}
	t.Cleanup(func() { _ = rdb.Close() })

	// A second client for inspecting the queue from the test goroutine.
	// Sharing the same client would work but the split makes the test
	// intent obvious and avoids any accidental coupling.
	inspectRDB, err := openRedis(redisURL)
	if err != nil {
		t.Fatalf("openRedis (inspect): %v", err)
	}
	t.Cleanup(func() { _ = inspectRDB.Close() })

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
		Port:                 18081,
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
	waitForListener(t, queueTestAPIAddr, 5*time.Second)
	t.Cleanup(func() {
		bgCancel()
		select {
		case <-errCh:
		case <-time.After(5 * time.Second):
			t.Logf("server did not exit within shutdown grace")
		}
		workers.Wait()
	})

	// ── 5. Seed org → api key → processor → artifact ─────────────────
	// Every domain table in the dev schema has RLS FORCED, so plain
	// pool connections cannot INSERT into organizations, api_keys,
	// artifacts, etc. Production handles this two ways: the request
	// path uses WithTenant to set app.current_org_id on a tx (so
	// the row's org_id matches and the policy passes), and the
	// auth path looks up api_keys by prefix without a tenant
	// scope — production relies on the orpheus role being a
	// superuser so FORCE RLS is bypassed automatically. The dev
	// docker stack, however, runs orpheus as a non-superuser, so
	// we have to flip app.is_service = 'true' on every connection
	// the test pool hands out. newServicePool wires that via the
	// pgx AfterConnect hook; queries inside WithTenant still scope
	// to the principal's org_id, and the auth lookup can find the
	// api_keys row we seed here.
	orgID := uuid.NewString()
	processorID := uuid.NewString()
	processorVersionID := uuid.NewString()
	artifactID := uuid.NewString()

	seedOrg(t, pool, orgID)
	apiKey := seedAPIKey(t, pool, orgID)
	seedProcessor(t, pool, processorID, processorVersionID)
	seedArtifact(t, pool, orgID, artifactID)

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
		_, _ = pool.Exec(cleanCtx, `DELETE FROM organizations WHERE id = $1`, orgID)
	})

	// ── 6. Start the Python Arq worker subprocess ─────────────────────
	workspaceRoot := findWorkspaceRoot(t)
	workerCmd := exec.CommandContext(bgCtx,
		"uv", "run", "--package", "orpheus-workers",
		"python", "-m", "orpheus_workers.worker",
	)
	workerCmd.Dir = workspaceRoot
	workerCmd.Env = append(os.Environ(), "ORPHEUS_WORKER_REDIS_URL="+redisURL)
	workerCmd.Stdout = io.Discard
	workerCmd.Stderr = io.Discard
	if err := workerCmd.Start(); err != nil {
		t.Fatalf("worker Start: %v", err)
	}
	t.Cleanup(func() {
		if workerCmd.Process != nil {
			_ = workerCmd.Process.Kill()
			_, _ = workerCmd.Process.Wait()
		}
	})

	// Give arq time to connect to Redis before we POST. The startup
	// window is sub-second on a warm cache but `uv run` adds a couple
	// of hundred ms of process-spawn overhead, so a 1 s wait is
	// comfortable without inflating the test budget noticeably.
	time.Sleep(1 * time.Second)

	// ── 7. POST /v1/jobs ─────────────────────────────────────────────
	body, _ := json.Marshal(handlers.CreateJobRequest{
		ArtifactID: artifactID,
		Processor:  handlers.ProcessorRef{Name: queueTestProcessorName, Version: queueTestProcessorVersion},
	})
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, queueTestAPIURL+"/v1/jobs", bytes.NewReader(body))
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

	// ── 8. Poll the arq queue until the worker drains it ─────────────
	deadline := time.Now().Add(queueTestWait)
	var lastLLen int64
	for {
		n, err := inspectRDB.LLen(ctx, queueTestArqQueueKey).Result()
		if err != nil {
			t.Fatalf("LLen(%s): %v", queueTestArqQueueKey, err)
		}
		lastLLen = n
		if n == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("arq queue not drained within %s (last LLEN = %d); outbox → NATS → Arq loop did not complete",
				queueTestWait, lastLLen)
		}
		time.Sleep(queueTestPollEvery)
	}
}

// ─────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────

// waitForListener polls a TCP address until the listener accepts or
// the deadline elapses. Mirrors the helper in e2e_test.go but
// duplicated here so this file is the single place a reader needs
// to look when reasoning about the queue test.
func waitForListener(t *testing.T, addr string, timeout time.Duration) {
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

// openRedis parses a redis:// URL via go-redis (handles userinfo,
// db number, rediss://) and returns a connected client.
func openRedis(rawURL string) (*redis.Client, error) {
	opts, err := redis.ParseURL(rawURL)
	if err != nil {
		return nil, fmt.Errorf("redis.parse: %w", err)
	}
	return redis.NewClient(opts), nil
}

// findWorkspaceRoot walks up from the current working directory
// until it finds a pyproject.toml. The test runs from the package
// directory (apps/api/internal/e2e/); the workspace root is the
// parent of apps/, which holds pyproject.toml for the uv workspace.
func findWorkspaceRoot(t *testing.T) string {
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

func seedOrg(t *testing.T, pool *db.DB, orgID string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO organizations (id, name, slug) VALUES ($1, $2, $3)`,
		orgID, "queue-e2e", "queue-e2e-"+orgID[:8],
	); err != nil {
		t.Fatalf("insert org: %v", err)
	}
}

// seedAPIKey generates an Argon2id-hashed api_key and returns the
// cleartext secret. The shape matches the production handler
// (apps/api/internal/handlers/api_keys.go): 32 random bytes, base64
// url-encoded, prefixed with "ak_live_". The prefix (first 9 chars)
// is the index column the validator looks up by.
func seedAPIKey(t *testing.T, pool *db.DB, orgID string) string {
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
		uuid.NewString(), orgID, "queue-e2e", hashed, prefix,
		[]string{"jobs:write", "jobs:read"},
	); err != nil {
		t.Fatalf("insert api_key: %v", err)
	}
	return secret
}

func seedProcessor(t *testing.T, pool *db.DB, processorID, versionID string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO processors (id, name, display_name, tier, timeout_seconds) VALUES ($1, $2, $3, 'cpu_tiny', 60)`,
		processorID, queueTestProcessorName, "Extract Metadata",
	); err != nil {
		t.Fatalf("insert processor: %v", err)
	}
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO processor_versions (id, processor_id, version, model_id, model_version_id) VALUES ($1, $2, $3, $4, $5)`,
		versionID, processorID, queueTestProcessorVersion, "model-extract-metadata", "v1",
	); err != nil {
		t.Fatalf("insert processor_version: %v", err)
	}
}

func seedArtifact(t *testing.T, pool *db.DB, orgID, artifactID string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO artifacts (id, org_id, s3_bucket, s3_key, sha256, size_bytes, content_type)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		artifactID, orgID, "queue-e2e-bucket", "queue-e2e/"+artifactID+".wav",
		"deadbeef", 1024, "audio/wav",
	); err != nil {
		t.Fatalf("insert artifact: %v", err)
	}
}

// newServicePool builds a *db.DB whose every connection has
// app.is_service = 'true' set on the session. The dev docker stack
// runs orpheus as a non-superuser, so FORCE RLS hides every row
// from a plain connection; flipping the GUC is the supported
// bypass (the production api key lookup and outbox publisher also
// assume it). Production code paths that go through WithTenant
// still set app.current_org_id on a tx, so the org_id scoping the
// tests rely on is unaffected.
func newServicePool(ctx context.Context, dsn string) (*db.DB, error) {
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
