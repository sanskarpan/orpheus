package outbox

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/orpheus/api/internal/db"
	"github.com/orpheus/api/internal/dbtx"
)

// TestEnqueueValidations covers the early-exit branches in [Enqueue].
// A nil DB or empty event_type/aggregate fields should fail before
// we touch the database; these checks are pure validation.
func TestEnqueueValidations(t *testing.T) {
	cases := []struct {
		name    string
		event   Event
		wantSub string
	}{
		{
			name:    "nil db",
			event:   Event{EventType: "x", AggregateType: "y", AggregateID: "z"},
			wantSub: "nil db",
		},
		{
			name:    "empty event_type",
			event:   Event{AggregateType: "upload", AggregateID: "abc"},
			wantSub: "empty event_type",
		},
		{
			name:    "empty aggregate type",
			event:   Event{EventType: "upload.complete", AggregateID: "abc"},
			wantSub: "incomplete",
		},
		{
			name:    "empty aggregate id",
			event:   Event{EventType: "upload.complete", AggregateType: "upload"},
			wantSub: "incomplete",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Enqueue(context.Background(), nil, tc.event)
			if err == nil {
				t.Fatal("Enqueue returned nil error, want error")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("err = %q, want substring %q", err.Error(), tc.wantSub)
			}
		})
	}
}

// TestNewEventID asserts that the random ID generator produces
// non-empty, non-equal values. We don't assert on the format because
// it can change — only on the contract.
func TestNewEventID(t *testing.T) {
	a := newEventID()
	b := newEventID()
	if a == "" || b == "" {
		t.Fatalf("newEventID returned empty: a=%q b=%q", a, b)
	}
	if a == b {
		t.Errorf("newEventID collision: %q", a)
	}
	if len(a) != 32 {
		t.Errorf("len(newEventID()) = %d, want 32 hex chars", len(a))
	}
}

// TestSubject verifies the public subject-formatting helper. A
// regression here would silently misroute every consumer.
func TestSubject(t *testing.T) {
	cases := map[string]string{
		"upload.complete": "adkil.upload.complete",
		"job.create":      "adkil.job.create",
		"x":               "adkil.x",
	}
	for in, want := range cases {
		if got := Subject(in); got != want {
			t.Errorf("Subject(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestPublisherRunReturnsOnCancel makes sure Run returns promptly when
// the context is cancelled, even with a nil DB / nil NATS. The
// production code never passes nil, but a malformed test
// configuration shouldn't hang.
func TestPublisherRunReturnsOnCancel(t *testing.T) {
	p := New(nil, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel so Run exits on its first iteration
	if err := p.Run(ctx); err != nil {
		t.Errorf("Run returned %v, want nil", err)
	}
}

// TestTickSkipsWhenWiringMissing confirms the safety net in tick:
// with nil DB or nil NATS, tick is a no-op rather than a panic.
func TestTickSkipsWhenWiringMissing(t *testing.T) {
	p := New(nil, nil, nil)
	// Should not panic.
	p.tick(context.Background())
}

// TestEnqueueRequiresDB is the live-DB version of the validation
// tests. It is skipped without a database. The intent is to catch
// wiring regressions (wrong column names, missing columns) in the
// INSERT path before integration tests in Phase 2.
func TestEnqueueRequiresDB(t *testing.T) {
	if testing.Short() {
		t.Skip("requires ORPHEUS_TEST_DATABASE_URL; skipped in -short mode")
	}
	t.Skip("ORPHEUS_TEST_DATABASE_URL not set; skipping live-db outbox test")
}

// TestEnqueue_InsideWithTenant_RollsBackWithTx is the load-bearing
// test for the dbtx threading in Enqueue. The flow:
//
//  1. Seed an org on a private service-role connection (RLS would
//     otherwise reject the insert).
//  2. Open a WithTenant(orgID) block. Inside the closure, call
//     outbox.Enqueue, then read the outbox table via dbtx.QueryRow
//     to confirm the row landed on the same connection. The closure
//     returns a deliberate error so WithTenant skips Commit and
//     runs the deferred Rollback.
//  3. After WithTenant returns, read the outbox table on the pool
//     and assert the row is gone.
//
// If a future refactor drops the dbtx.FromContext pick-up and
// reverts to using the pool, the in-closure read would still see
// the row (because the pool would have committed it before
// Rollback ran), and the post-closure read would find the row
// instead of zero. That is the regression this test exists to
// catch.
func TestEnqueue_InsideWithTenant_RollsBackWithTx(t *testing.T) {
	dsn := os.Getenv("ORPHEUS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ORPHEUS_TEST_DATABASE_URL not set; skipping live-DB outbox tx test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := db.Migrate(ctx, sqlDB); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	pool, err := db.New(ctx, dsn)
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	t.Cleanup(pool.Close)

	seedPool, err := db.New(ctx, dsn)
	if err != nil {
		t.Fatalf("db.New (seed): %v", err)
	}
	t.Cleanup(seedPool.Close)
	seedConn, err := seedPool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire seed conn: %v", err)
	}
	if _, err := seedConn.Exec(ctx, "SET app.is_service = 'true'"); err != nil {
		seedConn.Release()
		t.Fatalf("set service role: %v", err)
	}

	orgID := uuid.NewString()
	if _, err := seedConn.Exec(ctx,
		`INSERT INTO organizations (id, name, slug) VALUES ($1, $2, $3)`,
		orgID, "outbox-tx-test", "outbox-tx-"+orgID[:8],
	); err != nil {
		seedConn.Release()
		t.Fatalf("seed org: %v", err)
	}
	seedConn.Release()
	t.Cleanup(func() {
		cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanCancel()
		_, _ = pool.Exec(cleanCtx, `DELETE FROM outbox WHERE org_id = $1`, orgID)
		_, _ = pool.Exec(cleanCtx, `DELETE FROM organizations WHERE id = $1`, orgID)
	})

	sentinel := errors.New("deliberate rollback")
	rollbackErr := pool.WithTenant(ctx, orgID, func(ctx context.Context) error {
		if err := Enqueue(ctx, pool, Event{
			OrgID:         orgID,
			AggregateType: "job",
			AggregateID:   "agg-1",
			EventType:     "job.queued",
			Payload:       map[string]any{"job_id": "agg-1"},
		}); err != nil {
			return err
		}
		var n int
		if err := dbtx.QueryRow(ctx, pool, `SELECT count(*) FROM outbox WHERE org_id = $1`, orgID).Scan(&n); err != nil {
			return err
		}
		if n != 1 {
			t.Errorf("in-closure outbox count = %d, want 1 (row not visible inside tx)", n)
		}
		return sentinel
	})
	if !errors.Is(rollbackErr, sentinel) {
		t.Fatalf("WithTenant returned %v, want sentinel", rollbackErr)
	}

	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE org_id = $1`, orgID).Scan(&n); err != nil {
		t.Fatalf("post-rollback count: %v", err)
	}
	if n != 0 {
		t.Errorf("outbox count after rollback = %d, want 0 (enqueue did not honour the tx)", n)
	}
}
