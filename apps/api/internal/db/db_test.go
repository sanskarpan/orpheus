package db

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestNewSmoke opens a real pool against the DSN provided in
// ORPHEUS_TEST_DATABASE_URL and pings it. It is skipped in environments
// where no test database is available (CI without a Postgres service,
// local runs without docker compose up). The intent is to catch wiring
// regressions — a DSN typo, a missing extension, a TLS misconfig —
// before they reach integration tests in Phase 2.
func TestNewSmoke(t *testing.T) {
	dsn := os.Getenv("ORPHEUS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ORPHEUS_TEST_DATABASE_URL not set; skipping live-db smoke test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	pool, err := New(ctx, dsn)
	if err != nil {
		t.Fatalf("New() returned error: %v", err)
	}
	t.Cleanup(pool.Close)

	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("Ping() returned error: %v", err)
	}
}
