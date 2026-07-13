// Package e2e boots the Orpheus API against a real Postgres and drives
// a few HTTP requests through the public surface.
//
// The whole file is skipped unless ORPHEUS_E2E=1 is set, and only
// runs when ORPHEUS_TEST_DATABASE_URL points at a reachable
// database. CI without docker thus stays green; the test opt-in is
// the operator's signal that a Postgres service is up and the
// e2e flow is expected to pass.
//
// We deliberately do not bring up a test container in this file:
// testcontainers-go is not in go.mod and pulling it in just for one
// smoke test is not worth the new dep. Phase 1's contract is that
// "the test runs if you bring the DB; the test does not run if you
// don't" — that is what ORPHEUS_E2E=1 / ORPHEUS_TEST_DATABASE_URL
// gate on.
package e2e

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/orpheus/api/internal/config"
	"github.com/orpheus/api/internal/db"
	"github.com/orpheus/api/internal/metrics"
	"github.com/orpheus/api/internal/server"
)

// startServer wires a *server.Server to a fixed localhost port and
// returns the base URL plus a shutdown func. server.Run does not
// currently expose the bound port when Config.Port is 0, so we pin
// to a high-numbered test port. 18080 is unlikely to conflict with
// any other service on a CI box; if it does, the test fails with
// "address already in use" and the operator picks another one.
func startServer(t *testing.T, srv *server.Server, addr string) (string, func()) {
	t.Helper()
	if strings.HasSuffix(addr, ":0") {
		t.Fatalf("e2e test requires a fixed port; got %q", addr)
	}

	errCh := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		if err := srv.Run(ctx); err != nil {
			errCh <- err
		}
		close(errCh)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			_ = c.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("server did not start listening on %s within 2s", addr)
		}
		time.Sleep(20 * time.Millisecond)
	}

	shutdown := func() {
		cancel()
		select {
		case err := <-errCh:
			if err != nil {
				t.Logf("server exited with: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Logf("server did not exit within shutdown grace")
		}
	}
	return "http://" + addr, shutdown
}

// bootAPI is the shared setup for every test in this file. It skips
// the test when the env vars are not set, opens a sql.DB, runs the
// embedded migrations, and starts an in-process server.
//
// We exercise the public surface (server.New) only. Driving the /v1
// routes would require either standing up Keycloak or stubbing the
// auth package; both are out of scope for this smoke test. The
// e2e goal is "the binary boots, the migrations apply, the public
// routes serve" — server.New is exactly enough to prove that.
func bootAPI(t *testing.T) (baseURL string, pool *db.DB, shutdown func()) {
	t.Helper()
	if os.Getenv("ORPHEUS_E2E") != "1" {
		t.Skip("set ORPHEUS_E2E=1 to run")
	}
	dsn := os.Getenv("ORPHEUS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ORPHEUS_TEST_DATABASE_URL not set; cannot run e2e without a database")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

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

	pool, err = db.New(ctx, dsn)
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	t.Cleanup(pool.Close)

	cfg := &config.Config{
		Env:                  "test",
		Host:                 "127.0.0.1",
		Port:                 18080,
		ShutdownGraceSeconds: 5,
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := server.NewWithOptions(cfg, logger, server.Options{
		Metrics: metrics.New(),
	})
	baseURL, shutdown = startServer(t, srv, cfg.Addr())
	t.Cleanup(shutdown)
	return baseURL, pool, shutdown
}

func TestE2E_Health(t *testing.T) {
	baseURL, _, _ := bootAPI(t)
	client := &http.Client{Timeout: 5 * time.Second}

	t.Run("GET /health", func(t *testing.T) {
		resp, err := client.Get(baseURL + "/health")
		if err != nil {
			t.Fatalf("GET /health: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		if got := resp.Header.Get("Content-Type"); !strings.Contains(got, "application/json") {
			t.Errorf("Content-Type = %q, want application/json", got)
		}
		var body map[string]string
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if body["status"] != "ok" {
			t.Errorf("status = %q, want ok", body["status"])
		}
	})

	t.Run("GET /ready", func(t *testing.T) {
		resp, err := client.Get(baseURL + "/ready")
		if err != nil {
			t.Fatalf("GET /ready: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		var body struct {
			Status string            `json:"status"`
			Checks map[string]string `json:"checks"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if body.Status != "ready" {
			t.Errorf("status = %q, want ready", body.Status)
		}
		if body.Checks["service"] != "ok" {
			t.Errorf("checks.service = %q, want ok", body.Checks["service"])
		}
	})

	t.Run("GET /metrics", func(t *testing.T) {
		resp, err := client.Get(baseURL + "/metrics")
		if err != nil {
			t.Fatalf("GET /metrics: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		if got := resp.Header.Get("Content-Type"); !strings.Contains(got, "text/plain; version=0.0.4") {
			t.Errorf("Content-Type = %q, want text/plain; version=0.0.4", got)
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if !strings.Contains(string(body), "# HELP") {
			t.Errorf("metrics body does not contain # HELP")
		}
		if !strings.Contains(string(body), "orpheus_http_requests_total") {
			t.Errorf("metrics body does not contain orpheus_http_requests_total")
		}
		if !strings.Contains(string(body), "go_goroutines") {
			t.Errorf("metrics body does not contain go_goroutines")
		}
	})

	t.Run("GET /api/openapi.json", func(t *testing.T) {
		resp, err := client.Get(baseURL + "/api/openapi.json")
		if err != nil {
			t.Fatalf("GET /api/openapi.json: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		if got := resp.Header.Get("Content-Type"); !strings.Contains(got, "application/json") {
			t.Errorf("Content-Type = %q, want application/json", got)
		}
		var spec struct {
			OpenAPI string `json:"openapi"`
			Info    struct {
				Title string `json:"title"`
			} `json:"info"`
			Paths map[string]any `json:"paths"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&spec); err != nil {
			t.Fatalf("decode spec: %v", err)
		}
		if spec.Info.Title != "Orpheus API" {
			t.Errorf("info.title = %q, want Orpheus API", spec.Info.Title)
		}
		if spec.OpenAPI == "" {
			t.Error("openapi field is empty — spec is missing the version field")
		}
		// Pin the existence of the route families the contract covers.
		// The contract test for each path will catch a rename; this
		// e2e test just asserts the spec is mounted and serves.
		for _, prefix := range []string{"/v1/uploads", "/v1/jobs", "/v1/webhooks"} {
			if _, ok := spec.Paths[prefix]; !ok {
				t.Errorf("openapi spec missing path %q", prefix)
			}
		}
	})

	t.Run("Unknown path 404s", func(t *testing.T) {
		resp, err := client.Get(baseURL + "/does-not-exist")
		if err != nil {
			t.Fatalf("GET /does-not-exist: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("status = %d, want 404", resp.StatusCode)
		}
	})
}

func TestE2E_MigrationsApplyCleanly(t *testing.T) {
	// bootAPI already runs Migrate before starting the server, so this
	// test just confirms the schema is queryable afterwards. A
	// separate test so a migration failure is reported as such
	// rather than as a route-mounting error.
	_, pool, _ := bootAPI(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var name string
	if err := pool.QueryRow(ctx, "SELECT current_database()").Scan(&name); err != nil {
		t.Fatalf("SELECT current_database: %v", err)
	}
	if name == "" {
		t.Error("current_database() returned empty string")
	}
}
