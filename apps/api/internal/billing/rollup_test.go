package billing

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/orpheus/api/internal/db"
)

func TestCurrentMonth(t *testing.T) {
	start, end := CurrentMonth(time.Date(2026, 7, 16, 13, 30, 0, 0, time.UTC))
	if !start.Equal(time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("start = %v", start)
	}
	if !end.Equal(time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("end = %v", end)
	}
}

// servicePool opens a pool that sets app.is_service on every connection, so
// seed inserts bypass RLS the way the sweeper test does.
func servicePool(t *testing.T, dsn string) *db.DB {
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
	p, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("NewWithConfig: %v", err)
	}
	t.Cleanup(p.Close)
	return &db.DB{Pool: p}
}

// TestRollupPeriod seeds two completed jobs in a period and asserts the
// rollup writes one invoice per org with the summed cost + count, that a
// re-run is idempotent, and that a paid invoice is not overwritten.
func TestRollupPeriod(t *testing.T) {
	dsn := os.Getenv("ORPHEUS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ORPHEUS_TEST_DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	pool, err := db.New(ctx, dsn) // plain pool; rollup sets is_service itself
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	t.Cleanup(pool.Close)
	svc := servicePool(t, dsn)

	orgID := uuid.NewString()
	if _, err := svc.Exec(ctx, `INSERT INTO organizations (id,name,slug) VALUES ($1,$2,$2)`, orgID, "bill-"+orgID); err != nil {
		t.Fatalf("seed org: %v", err)
	}

	// Period: a fixed month well clear of "now" so no other rows collide.
	start := time.Date(2099, 3, 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 1, 0)
	mid := start.AddDate(0, 0, 10)

	seedJob := func(cost float64, started, completed time.Time, status string) {
		_, err := svc.Exec(ctx, `
			INSERT INTO jobs (id, org_id, job_type, status, cost_usd, started_at, completed_at)
			VALUES ($1, $2, 'transcribe', $3::job_status, $4, $5, $6)
		`, uuid.NewString(), orgID, status, cost, started, completed)
		if err != nil {
			t.Fatalf("seed job: %v", err)
		}
	}
	seedJob(1.50, mid, mid.Add(30*time.Second), "completed")
	seedJob(2.25, mid, mid.Add(45*time.Second), "completed")
	// A failed job and an out-of-window job must be excluded.
	seedJob(9.99, mid, mid.Add(time.Minute), "failed")
	seedJob(9.99, end.Add(time.Hour), end.Add(time.Hour+time.Minute), "completed")

	r := NewRollup(pool, nil)
	n, err := r.RollupPeriod(ctx, start, end)
	if err != nil {
		t.Fatalf("RollupPeriod: %v", err)
	}
	if n != 1 {
		t.Fatalf("rows affected = %d, want 1", n)
	}

	var (
		jobsCount int
		total     float64
		compute   float64
		status    string
	)
	if err := svc.QueryRow(ctx, `
		SELECT jobs_count, total_usd::float8, compute_seconds::float8, status
		FROM invoices WHERE org_id = $1 AND period_start = $2
	`, orgID, start).Scan(&jobsCount, &total, &compute, &status); err != nil {
		t.Fatalf("read invoice: %v", err)
	}
	if jobsCount != 2 || total < 3.7499 || total > 3.7501 {
		t.Fatalf("invoice = count %d total %v, want 2 / 3.75", jobsCount, total)
	}
	if compute < 74.9 || compute > 75.1 {
		t.Errorf("compute_seconds = %v, want ~75", compute)
	}
	if status != "open" {
		t.Errorf("status = %q, want open", status)
	}

	// Idempotent re-run: still one invoice, same totals.
	if _, err := r.RollupPeriod(ctx, start, end); err != nil {
		t.Fatalf("re-run: %v", err)
	}
	var count int
	if err := svc.QueryRow(ctx, `SELECT COUNT(*) FROM invoices WHERE org_id = $1 AND period_start = $2`, orgID, start).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("invoice count after re-run = %d, want 1", count)
	}

	// A paid invoice must not be overwritten by a later rollup.
	if _, err := svc.Exec(ctx, `UPDATE invoices SET status='paid', paid_at=now() WHERE org_id=$1 AND period_start=$2`, orgID, start); err != nil {
		t.Fatalf("mark paid: %v", err)
	}
	seedJob(5.00, mid, mid.Add(10*time.Second), "completed")
	if _, err := r.RollupPeriod(ctx, start, end); err != nil {
		t.Fatalf("rollup after paid: %v", err)
	}
	if err := svc.QueryRow(ctx, `SELECT status, total_usd::float8 FROM invoices WHERE org_id=$1 AND period_start=$2`, orgID, start).Scan(&status, &total); err != nil {
		t.Fatalf("read paid invoice: %v", err)
	}
	if status != "paid" || total < 3.7499 || total > 3.7501 {
		t.Fatalf("paid invoice mutated: status %q total %v", status, total)
	}
}
