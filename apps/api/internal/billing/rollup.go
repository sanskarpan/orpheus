package billing

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/orpheus/api/internal/db"
)

const defaultRollupInterval = 6 * time.Hour

// Rollup aggregates per-job cost into per-org invoices for a billing period.
// It is a system component: like the retention sweeper it runs with the
// service role in transaction-local scope so it can read/write every org's
// FORCE-RLS rows without a request principal.
type Rollup struct {
	DB       *db.DB
	Logger   *slog.Logger
	Interval time.Duration
}

// NewRollup constructs a Rollup with sensible defaults.
func NewRollup(database *db.DB, logger *slog.Logger) *Rollup {
	if logger == nil {
		logger = slog.Default()
	}
	return &Rollup{DB: database, Logger: logger, Interval: defaultRollupInterval}
}

// Run loops until ctx is cancelled, rolling up the current calendar month on
// each tick. Re-running is safe: RollupPeriod upserts, and never overwrites a
// paid invoice.
func (r *Rollup) Run(ctx context.Context) error {
	interval := r.Interval
	if interval <= 0 {
		interval = defaultRollupInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	r.Logger.Info("billing.rollup.started", "interval", interval.String())
	for {
		select {
		case <-ctx.Done():
			r.Logger.Info("billing.rollup.stopped")
			return nil
		case <-t.C:
			start, end := CurrentMonth(time.Now())
			if n, err := r.RollupPeriod(ctx, start, end); err != nil {
				r.Logger.Error("billing.rollup_failed", "err", err)
			} else if n > 0 {
				r.Logger.Info("billing.rolled_up", "invoices", n, "period_start", start.Format(time.RFC3339))
			}
		}
	}
}

// CurrentMonth returns the [start, end) bounds of the UTC calendar month
// containing t. The rollup and the /v1/usage endpoint agree on this window.
func CurrentMonth(t time.Time) (time.Time, time.Time) {
	u := t.UTC()
	start := time.Date(u.Year(), u.Month(), 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 1, 0)
	return start, end
}

// RollupPeriod aggregates every org's completed jobs whose completed_at falls
// in [start, end) into an invoice row per org, and returns how many invoice
// rows were written. It runs in a single service-role transaction.
//
// The upsert refreshes an existing draft/open invoice in place but leaves a
// paid/void/failed invoice untouched, so a late-arriving job cannot silently
// mutate an already-collected bill.
func (r *Rollup) RollupPeriod(ctx context.Context, start, end time.Time) (int, error) {
	if r.DB == nil {
		return 0, nil
	}
	var n int
	err := r.withServiceTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			INSERT INTO invoices (
				org_id, period_start, period_end,
				jobs_count, compute_seconds, total_usd, status
			)
			SELECT
				org_id,
				$1::timestamptz,
				$2::timestamptz,
				COUNT(*)::int,
				COALESCE(SUM(EXTRACT(EPOCH FROM (completed_at - started_at))), 0),
				COALESCE(SUM(cost_usd), 0),
				'open'
			FROM jobs
			WHERE status = 'completed'
			  AND completed_at >= $1
			  AND completed_at <  $2
			GROUP BY org_id
			ON CONFLICT (org_id, period_start) DO UPDATE SET
				period_end      = EXCLUDED.period_end,
				jobs_count      = EXCLUDED.jobs_count,
				compute_seconds = EXCLUDED.compute_seconds,
				total_usd       = EXCLUDED.total_usd,
				updated_at      = now()
			WHERE invoices.status IN ('draft', 'open')
		`, start, end)
		if err != nil {
			return err
		}
		n = int(tag.RowsAffected())
		return nil
	})
	return n, err
}

// withServiceTx runs fn in a transaction with app.is_service set locally, so
// the rollup can touch every org's FORCE-RLS rows.
func (r *Rollup) withServiceTx(ctx context.Context, fn func(pgx.Tx) error) error {
	conn, err := r.DB.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()
	if _, err := tx.Exec(ctx, "SELECT set_config('app.is_service','true',true)"); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	committed = true
	return nil
}
