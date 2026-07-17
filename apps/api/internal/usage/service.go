// Package usage rolls completed-job cost into an hourly analytics store and
// fires budget threshold alerts (PRD 07). It is a system component: a polling
// loop under the service role (the rollup/budget tables are FORCE-RLS).
//
// Idempotent by design: the rollup re-aggregates a trailing window with an
// upsert, and each (budget, period, threshold) alert is deduped by a unique
// constraint so it fires exactly once per period.
package usage

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/orpheus/api/internal/db"
)

const (
	defaultInterval = time.Minute
	// Re-aggregate this trailing window each tick so late-completing jobs land
	// in the right hour; older hours are already final.
	rollupWindow = 48 * time.Hour
)

// Service runs the rollup + budget checks.
type Service struct {
	DB       *db.DB
	Logger   *slog.Logger
	Interval time.Duration
}

// New builds a Service.
func New(database *db.DB, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{DB: database, Logger: logger, Interval: defaultInterval}
}

// Run loops until ctx is cancelled.
func (s *Service) Run(ctx context.Context) error {
	interval := s.Interval
	if interval <= 0 {
		interval = defaultInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	s.Logger.Info("usage.started", "interval", interval.String())
	for {
		select {
		case <-ctx.Done():
			s.Logger.Info("usage.stopped")
			return nil
		case <-t.C:
			if err := s.RollupOnce(ctx); err != nil {
				s.Logger.Error("usage.rollup_failed", "err", err)
			}
			if err := s.CheckBudgets(ctx); err != nil {
				s.Logger.Error("usage.budget_check_failed", "err", err)
			}
		}
	}
}

// RollupOnce re-aggregates the trailing window into usage_rollup_hourly for the
// total / processor / status dimensions.
func (s *Service) RollupOnce(ctx context.Context) error {
	if s.DB == nil {
		return nil
	}
	return s.withServiceTx(ctx, func(tx pgx.Tx) error {
		// total + processor: cost-bearing completed jobs.
		for _, q := range []string{
			`INSERT INTO usage_rollup_hourly (org_id, hour, dimension, dimension_value, jobs, compute_seconds, cost_usd)
			 SELECT org_id, date_trunc('hour', completed_at), 'total', '',
			        count(*), COALESCE(sum(extract(epoch from (completed_at-started_at))),0), COALESCE(sum(cost_usd),0)
			 FROM jobs WHERE status='completed' AND completed_at >= now() - $1::interval
			 GROUP BY 1,2
			 ON CONFLICT (org_id,hour,dimension,dimension_value) DO UPDATE
			   SET jobs=EXCLUDED.jobs, compute_seconds=EXCLUDED.compute_seconds, cost_usd=EXCLUDED.cost_usd`,
			`INSERT INTO usage_rollup_hourly (org_id, hour, dimension, dimension_value, jobs, compute_seconds, cost_usd)
			 SELECT org_id, date_trunc('hour', completed_at), 'processor', COALESCE(params->'_processor'->>'name','custom'),
			        count(*), COALESCE(sum(extract(epoch from (completed_at-started_at))),0), COALESCE(sum(cost_usd),0)
			 FROM jobs WHERE status='completed' AND completed_at >= now() - $1::interval
			 GROUP BY 1,2,4
			 ON CONFLICT (org_id,hour,dimension,dimension_value) DO UPDATE
			   SET jobs=EXCLUDED.jobs, compute_seconds=EXCLUDED.compute_seconds, cost_usd=EXCLUDED.cost_usd`,
			`INSERT INTO usage_rollup_hourly (org_id, hour, dimension, dimension_value, jobs, compute_seconds, cost_usd)
			 SELECT org_id, date_trunc('hour', completed_at), 'status', status::text,
			        count(*), 0, COALESCE(sum(cost_usd),0)
			 FROM jobs WHERE completed_at >= now() - $1::interval AND status IN ('completed','failed','canceled','dead_letter')
			 GROUP BY 1,2,4
			 ON CONFLICT (org_id,hour,dimension,dimension_value) DO UPDATE
			   SET jobs=EXCLUDED.jobs, cost_usd=EXCLUDED.cost_usd`,
		} {
			if _, err := tx.Exec(ctx, q, rollupWindow.String()); err != nil {
				return err
			}
		}
		return nil
	})
}

type budgetRow struct {
	id, orgID, scope, scopeID string
	limit                     float64
	thresholds                []float64
}

// CheckBudgets fires an alert for each newly-crossed threshold.
func (s *Service) CheckBudgets(ctx context.Context) error {
	if s.DB == nil {
		return nil
	}
	var budgets []budgetRow
	if err := s.withServiceTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id::text, org_id::text, scope, COALESCE(scope_id,''), limit_usd::float8, alert_thresholds FROM budgets`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var b budgetRow
			if err := rows.Scan(&b.id, &b.orgID, &b.scope, &b.scopeID, &b.limit, &b.thresholds); err != nil {
				return err
			}
			budgets = append(budgets, b)
		}
		return rows.Err()
	}); err != nil {
		return err
	}

	for _, b := range budgets {
		spend, periodStart, err := s.BudgetSpend(ctx, b.orgID, b.scope, b.scopeID)
		if err != nil || b.limit <= 0 {
			continue
		}
		for _, thr := range b.thresholds {
			if spend < thr*b.limit {
				continue
			}
			_ = s.withServiceTx(ctx, func(tx pgx.Tx) error {
				// Dedup by unique(budget_id, period_start, threshold): only the
				// first insert per period+threshold fires the event.
				tag, e := tx.Exec(ctx, `
					INSERT INTO budget_alerts (org_id, budget_id, period_start, threshold, spend_usd)
					VALUES ($1,$2,$3,$4,$5) ON CONFLICT (budget_id, period_start, threshold) DO NOTHING
				`, b.orgID, b.id, periodStart, thr, spend)
				if e != nil {
					return e
				}
				if tag.RowsAffected() == 0 {
					return nil // already fired this period
				}
				payload, _ := json.Marshal(map[string]any{
					"budget_id": b.id, "threshold": thr, "spend_usd": spend, "limit_usd": b.limit,
				})
				_, e = tx.Exec(ctx, `
					INSERT INTO outbox (id, org_id, aggregate_type, aggregate_id, event_type, payload, headers)
					VALUES (gen_random_uuid(), $1, 'budget', $2, 'usage.budget_threshold', $3::jsonb, '{}'::jsonb)
				`, b.orgID, b.id, payload)
				return e
			})
		}
	}
	return nil
}

// BudgetSpend returns the current-period spend for a budget and the period
// start. Monthly period only in v1.
func (s *Service) BudgetSpend(ctx context.Context, orgID, scope, scopeID string) (float64, time.Time, error) {
	var spend float64
	var periodStart time.Time
	err := s.withServiceTx(ctx, func(tx pgx.Tx) error {
		if scope == "processor" {
			return tx.QueryRow(ctx, `
				SELECT COALESCE(sum(cost_usd),0)::float8, date_trunc('month', now())
				FROM usage_rollup_hourly
				WHERE org_id=$1 AND dimension='processor' AND dimension_value=$2 AND hour >= date_trunc('month', now())
			`, orgID, scopeID).Scan(&spend, &periodStart)
		}
		return tx.QueryRow(ctx, `
			SELECT COALESCE(sum(cost_usd),0)::float8, date_trunc('month', now())
			FROM usage_rollup_hourly
			WHERE org_id=$1 AND dimension='total' AND hour >= date_trunc('month', now())
		`, orgID).Scan(&spend, &periodStart)
	})
	return spend, periodStart, err
}

func (s *Service) withServiceTx(ctx context.Context, fn func(pgx.Tx) error) error {
	conn, err := s.DB.Acquire(ctx)
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
