package usage

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/orpheus/api/internal/db"
)

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

// TestUsage_RollupAndBudgetAlert seeds completed jobs, rolls them up, and
// verifies a budget over-limit fires one alert per threshold (deduped).
func TestUsage_RollupAndBudgetAlert(t *testing.T) {
	dsn := os.Getenv("ORPHEUS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ORPHEUS_TEST_DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	svc := servicePool(t, dsn)
	service := New(svc, slog.New(slog.NewTextHandler(io.Discard, nil)))

	orgID := uuid.NewString()
	if _, err := svc.Exec(ctx, `INSERT INTO organizations (id,name,slug) VALUES ($1,$2,$2)`, orgID, "u-"+orgID); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	t.Cleanup(func() {
		c, cc := context.WithTimeout(context.Background(), 5*time.Second)
		defer cc()
		_, _ = svc.Exec(c, `DELETE FROM budget_alerts WHERE org_id=$1`, orgID)
		_, _ = svc.Exec(c, `DELETE FROM budgets WHERE org_id=$1`, orgID)
		_, _ = svc.Exec(c, `DELETE FROM usage_rollup_hourly WHERE org_id=$1`, orgID)
		_, _ = svc.Exec(c, `DELETE FROM outbox WHERE org_id=$1`, orgID)
		_, _ = svc.Exec(c, `DELETE FROM jobs WHERE org_id=$1`, orgID)
		_, _ = svc.Exec(c, `DELETE FROM organizations WHERE id=$1`, orgID)
	})
	// Two completed transcribe jobs, cost 0.6 + 0.6 = 1.2.
	for i := 0; i < 2; i++ {
		if _, err := svc.Exec(ctx, `
			INSERT INTO jobs (id,org_id,job_type,params,status,cost_usd,started_at,completed_at)
			VALUES ($1,$2,'custom'::job_type,'{"_processor":{"name":"transcribe"}}'::jsonb,'completed'::job_status,0.6,now()-interval '30 seconds',now())
		`, uuid.NewString(), orgID); err != nil {
			t.Fatalf("seed job: %v", err)
		}
	}

	// 1) Rollup → total + processor rows reflect cost 1.2 / 2 jobs.
	if err := service.RollupOnce(ctx); err != nil {
		t.Fatalf("rollup: %v", err)
	}
	var totalCost float64
	var totalJobs int
	if err := svc.QueryRow(ctx, `SELECT COALESCE(sum(cost_usd),0)::float8, COALESCE(sum(jobs),0)::int FROM usage_rollup_hourly WHERE org_id=$1 AND dimension='total'`, orgID).Scan(&totalCost, &totalJobs); err != nil {
		t.Fatalf("read total: %v", err)
	}
	if totalCost < 1.19 || totalCost > 1.21 || totalJobs != 2 {
		t.Fatalf("total rollup = cost %v jobs %d, want 1.2/2", totalCost, totalJobs)
	}
	var procCost float64
	if err := svc.QueryRow(ctx, `SELECT COALESCE(sum(cost_usd),0)::float8 FROM usage_rollup_hourly WHERE org_id=$1 AND dimension='processor' AND dimension_value='transcribe'`, orgID).Scan(&procCost); err != nil {
		t.Fatalf("read processor: %v", err)
	}
	if procCost < 1.19 || procCost > 1.21 {
		t.Fatalf("processor rollup cost = %v, want 1.2", procCost)
	}

	// 2) Budget limit 1.0 (spend 1.2 → over 0.5, 0.8, 1.0). Alerts fire once each.
	budgetID := uuid.NewString()
	if _, err := svc.Exec(ctx, `INSERT INTO budgets (id,org_id,scope,period,limit_usd,alert_thresholds,enforcement) VALUES ($1,$2,'org','monthly',1.0,'{0.5,0.8,1.0}',$3)`,
		budgetID, orgID, "alert"); err != nil {
		t.Fatalf("seed budget: %v", err)
	}
	if err := service.CheckBudgets(ctx); err != nil {
		t.Fatalf("check budgets: %v", err)
	}
	var alertCount, eventCount int
	if err := svc.QueryRow(ctx, `SELECT count(*) FROM budget_alerts WHERE budget_id=$1`, budgetID).Scan(&alertCount); err != nil {
		t.Fatalf("count alerts: %v", err)
	}
	if alertCount != 3 {
		t.Fatalf("alerts=%d, want 3 (0.5,0.8,1.0 all crossed)", alertCount)
	}
	if err := svc.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE org_id=$1 AND event_type='usage.budget_threshold'`, orgID).Scan(&eventCount); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if eventCount != 3 {
		t.Fatalf("outbox events=%d, want 3", eventCount)
	}

	// 3) Idempotent: re-check does not re-fire.
	if err := service.CheckBudgets(ctx); err != nil {
		t.Fatalf("recheck: %v", err)
	}
	if err := svc.QueryRow(ctx, `SELECT count(*) FROM budget_alerts WHERE budget_id=$1`, budgetID).Scan(&alertCount); err != nil {
		t.Fatalf("recount alerts: %v", err)
	}
	if alertCount != 3 {
		t.Fatalf("alerts re-fired: %d, want 3", alertCount)
	}
}
