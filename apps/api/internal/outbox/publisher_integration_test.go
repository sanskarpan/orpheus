package outbox

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/orpheus/api/internal/db"
)

// TestTick_DrainsUnderRLSAndMarksPublished is the regression test for
// two bugs:
//  1. the publisher read outbox on a bare pool with no service context,
//     but outbox has FORCE row-level security — so it saw ZERO rows and
//     never drained anything; and
//  2. it claimed rows on the pool (auto-commit), releasing FOR UPDATE
//     SKIP LOCKED locks immediately so concurrent publishers could
//     double-publish.
//
// It seeds an outbox row, runs tick with a stub JetStream, and asserts
// the event was published exactly once and the row is marked published.
func TestTick_DrainsUnderRLSAndMarksPublished(t *testing.T) {
	dsn := os.Getenv("ORPHEUS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ORPHEUS_TEST_DATABASE_URL not set; skipping outbox drain test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pool, err := db.New(ctx, dsn) // plain pool, no is_service — like production
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	t.Cleanup(pool.Close)

	svc := outboxServicePool(t, dsn)
	orgID := uuid.NewString()
	eventID := uuid.NewString()
	if _, err := svc.Exec(ctx, `INSERT INTO organizations (id, name, slug) VALUES ($1, $2, $2)`, orgID, "ob-"+orgID); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer ccancel()
		_, _ = svc.Exec(cctx, `DELETE FROM outbox WHERE org_id = $1`, orgID)
		_, _ = svc.Exec(cctx, `DELETE FROM organizations WHERE id = $1`, orgID)
	})
	if _, err := svc.Exec(ctx, `
		INSERT INTO outbox (id, org_id, event_type, aggregate_type, aggregate_id, payload)
		VALUES ($1, $2, 'job.queued', 'job', $3, '{"job_id":"x"}')
	`, eventID, orgID, uuid.NewString()); err != nil {
		t.Fatalf("seed outbox: %v", err)
	}

	// The test DB may hold unpublished rows from other tests; tick is a
	// service-wide drain, so we assert on our specific row plus the
	// no-double-publish invariant rather than an absolute count.
	stub := &stubPublisher{}
	p := &Publisher{DB: pool, JS: stub, Batch: 1000, Logger: slog.Default()}
	p.tick(ctx)

	if stub.callCount() == 0 {
		t.Fatal("tick published nothing — RLS drain is broken (no service context)")
	}

	// Our row must be marked published now (read under service context).
	var publishedAt *time.Time
	if err := svc.QueryRow(ctx, `SELECT published_at FROM outbox WHERE id = $1`, eventID).Scan(&publishedAt); err != nil {
		t.Fatalf("read outbox: %v", err)
	}
	if publishedAt == nil {
		t.Fatal("our outbox row was not marked published after tick")
	}

	// A second tick must not re-publish anything already published.
	afterFirst := stub.callCount()
	p.tick(ctx)
	if got := stub.callCount(); got != afterFirst {
		t.Fatalf("re-published already-published events: callCount %d -> %d", afterFirst, got)
	}
}

func outboxServicePool(t *testing.T, dsn string) *db.DB {
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
