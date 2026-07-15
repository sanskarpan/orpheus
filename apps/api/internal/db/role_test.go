package db

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestAssertTenantSafeRole verifies the RLS-bypass guard: the app's
// dedicated role must be neither superuser nor BYPASSRLS. This runs
// against the configured test DB; it is skipped without one.
func TestAssertTenantSafeRole(t *testing.T) {
	dsn := os.Getenv("ORPHEUS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ORPHEUS_TEST_DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := New(ctx, dsn)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(pool.Close)

	// The app role must be tenant-safe. If this fails, the test DB is
	// misconfigured (running the app as a superuser) — which is exactly
	// the condition the assertion is meant to catch in prod.
	if err := pool.AssertTenantSafeRole(ctx); err != nil {
		t.Fatalf("AssertTenantSafeRole: %v", err)
	}
}
