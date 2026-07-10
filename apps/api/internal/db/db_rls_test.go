package db

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/orpheus/api/internal/dbtx"
)

// TestWithTenant_RLSLoadBearing proves that the WithTenant GUC
// (app.current_org_id) is now set on the *same* connection every
// query inside the closure runs on. Concretely:
//
//  1. Two orgs are seeded, each with one job. dbtx.QueryRow inside
//     WithTenant(orgA, ...) returns *only* orgA's row. A future
//     regression that pulled the tx out of ctx and fell back to the
//     pool for the query would expose both rows and fail the test.
//
//  2. A cross-tenant INSERT inside WithTenant(orgA, ...) is denied
//     by the jobs_tenant_insert WITH CHECK policy. The helper runs
//     the insert on the same connection the GUC was set on, so the
//     RLS check fires; with the helper on the pool, the policy is
//     bypassed and the insert would succeed.
//
// Gated on ORPHEUS_TEST_DATABASE_URL; the test skips otherwise.
func TestWithTenant_RLSLoadBearing(t *testing.T) {
	dsn := os.Getenv("ORPHEUS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ORPHEUS_TEST_DATABASE_URL not set; skipping live-DB RLS test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Apply migrations on a short-lived database/sql handle. The
	// migrations are idempotent so this is safe to call against a
	// database that has already been migrated.
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := Migrate(ctx, sqlDB); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	pool, err := New(ctx, dsn)
	if err != nil {
		t.Fatalf("New() returned error: %v", err)
	}
	t.Cleanup(pool.Close)

	// Seed inserts on a *separate* pool so the
	// app.is_service SET never lands on a connection the SUT
	// (the main `pool`) might acquire. SET on a pooled
	// connection would persist for the next caller and bypass
	// RLS for them; a private pool we close at end-of-test is
	// the only safe way to do service-role work alongside a
	// non-service test body.
	seedPool, err := New(ctx, dsn)
	if err != nil {
		t.Fatalf("New(seed pool): %v", err)
	}
	seedConn, err := seedPool.Acquire(ctx)
	if err != nil {
		seedPool.Close()
		t.Fatalf("acquire seed conn: %v", err)
	}
	if _, err := seedConn.Exec(ctx, "SET app.is_service = 'true'"); err != nil {
		seedConn.Release()
		seedPool.Close()
		t.Fatalf("set service role: %v", err)
	}

	orgA := uuid.NewString()
	orgB := uuid.NewString()
	jobA := uuid.NewString()
	jobB := uuid.NewString()

	if _, err := seedConn.Exec(ctx,
		`INSERT INTO organizations (id, name, slug) VALUES ($1, $2, $3)`,
		orgA, "rls-test-a", "rls-test-a-"+orgA[:8],
	); err != nil {
		seedConn.Release()
		seedPool.Close()
		t.Fatalf("seed orgA: %v", err)
	}
	if _, err := seedConn.Exec(ctx,
		`INSERT INTO organizations (id, name, slug) VALUES ($1, $2, $3)`,
		orgB, "rls-test-b", "rls-test-b-"+orgB[:8],
	); err != nil {
		seedConn.Release()
		seedPool.Close()
		t.Fatalf("seed orgB: %v", err)
	}
	if _, err := seedConn.Exec(ctx,
		`INSERT INTO jobs (id, org_id, job_type) VALUES ($1, $2, 'extract-metadata')`,
		jobA, orgA,
	); err != nil {
		seedConn.Release()
		seedPool.Close()
		t.Fatalf("seed jobA: %v", err)
	}
	if _, err := seedConn.Exec(ctx,
		`INSERT INTO jobs (id, org_id, job_type) VALUES ($1, $2, 'extract-metadata')`,
		jobB, orgB,
	); err != nil {
		seedConn.Release()
		seedPool.Close()
		t.Fatalf("seed jobB: %v", err)
	}
	seedConn.Release()
	seedPool.Close()
	t.Cleanup(func() {
		cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanCancel()
		_, _ = pool.Exec(cleanCtx, `DELETE FROM jobs WHERE id IN ($1, $2)`, jobA, jobB)
		_, _ = pool.Exec(cleanCtx, `DELETE FROM organizations WHERE id IN ($1, $2)`, orgA, orgB)
	})

	// Read inside WithTenant(orgA) must see exactly one row.
	var n int
	err = pool.WithTenant(ctx, orgA, func(ctx context.Context) error {
		return dbtx.QueryRow(ctx, pool, `SELECT count(*) FROM jobs`).Scan(&n)
	})
	if err != nil {
		t.Fatalf("WithTenant(orgA) count(*): %v", err)
	}
	if n != 1 {
		t.Errorf("WithTenant(orgA) jobs count = %d, want 1 (RLS should hide orgB)", n)
	}

	// Cross-tenant INSERT inside WithTenant(orgA) must be denied.
	crossJobID := uuid.NewString()
	crossErr := pool.WithTenant(ctx, orgA, func(ctx context.Context) error {
		_, err := dbtx.Exec(ctx, pool,
			`INSERT INTO jobs (id, org_id, job_type) VALUES ($1, $2, 'extract-metadata')`,
			crossJobID, orgB,
		)
		return err
	})
	if crossErr == nil {
		t.Fatalf("cross-tenant INSERT succeeded; RLS is not load-bearing")
	}
	if !strings.Contains(strings.ToLower(crossErr.Error()), "row-level security") {
		t.Errorf("cross-tenant INSERT error = %v, want a row-level security violation", crossErr)
	}
	// Sanity: the cross-tenant row must not have been written.
	var crossExists int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE id = $1`, crossJobID).Scan(&crossExists); err != nil {
		t.Fatalf("post-check count: %v", err)
	}
	if crossExists != 0 {
		t.Errorf("cross-tenant row leaked into jobs; count = %d", crossExists)
	}
}
