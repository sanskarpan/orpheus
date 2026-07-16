package audit

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/orpheus/api/internal/auth"
	"github.com/orpheus/api/internal/db"
	"github.com/orpheus/api/internal/dbtx"
)

// TestRecord_WritesRowUnderRLS is the regression test for two bugs:
//  1. the action format was inverted (`create.upload`) vs the
//     audit_action enum (`upload.create`), so every insert failed the
//     enum cast; and
//  2. Record ran the INSERT on the bare pool with no tenant GUC, so the
//     audit_log FORCE-RLS insert policy rejected every row.
//
// It drives the real Record path against Postgres and reads the row back
// under WithTenant, proving the audit row is actually persisted.
func TestRecord_WritesRowUnderRLS(t *testing.T) {
	dsn := os.Getenv("ORPHEUS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ORPHEUS_TEST_DATABASE_URL not set; skipping live-db audit test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Tenant-scoped pool for the Recorder (no is_service): RLS is
	// load-bearing, so this proves Record sets the org GUC itself.
	pool, err := db.New(ctx, dsn)
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	t.Cleanup(pool.Close)

	// Service pool to seed the org + user across RLS.
	svc := servicesPool(t, dsn)
	orgID := uuid.NewString()
	userID := uuid.NewString()
	if _, err := svc.Exec(ctx, `INSERT INTO organizations (id, name, slug) VALUES ($1, $2, $2)`, orgID, "audit-"+orgID); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	t.Cleanup(func() { _, _ = svc.Exec(context.Background(), `DELETE FROM organizations WHERE id = $1`, orgID) })
	if _, err := svc.Exec(ctx, `INSERT INTO users (id, org_id, email, name) VALUES ($1, $2, $3, 'a')`, userID, orgID, userID[:8]+"@t"); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	rec := New(pool, nil)
	resourceID := uuid.NewString()
	ctx = auth.WithPrincipal(ctx, &auth.Principal{OrgID: orgID, UserID: userID})
	if err := rec.Record(ctx, Entry{
		Action:       "upload.create",
		ResourceType: "upload",
		ResourceID:   resourceID,
		Metadata:     map[string]any{"filename": "clip.wav"},
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	// Read the row back under the tenant scope.
	var action, resourceType string
	if err := pool.WithTenant(ctx, orgID, func(tctx context.Context) error {
		return dbtx.QueryRow(tctx, pool, `
			SELECT action::text, resource_type FROM audit_log
			WHERE org_id = $1 AND resource_id = $2
		`, orgID, resourceID).Scan(&action, &resourceType)
	}); err != nil {
		t.Fatalf("read audit row: %v", err)
	}
	if action != "upload.create" || resourceType != "upload" {
		t.Errorf("audit row = (%q, %q), want (upload.create, upload)", action, resourceType)
	}
}

func servicesPool(t *testing.T, dsn string) *db.DB {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SET app.is_service = 'true'")
		return err
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("NewWithConfig: %v", err)
	}
	t.Cleanup(pool.Close)
	return &db.DB{Pool: pool}
}
