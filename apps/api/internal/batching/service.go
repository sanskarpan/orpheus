// Package batching aggregates tracked batches and pushes per-job results to a
// tenant's S3 destination (PRD 06). It is a system component: a polling loop
// that runs with the service role (batches/jobs are FORCE-RLS) so it can act
// across every org without a request principal.
//
// The design is idempotent by recomputation: batch counts are derived from the
// child jobs on every tick, and each of {result push, batch.completed
// callback} is guarded by a per-row flag so at-least-once ticking never
// double-acts.
package batching

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/orpheus/api/internal/db"
	"github.com/orpheus/api/internal/delivery"
	"github.com/orpheus/api/internal/storage/s3"
)

const (
	defaultInterval = 5 * time.Second
	pushBatch       = 100
)

// Service aggregates batches and delivers results.
type Service struct {
	DB        *db.DB
	Deliverer *delivery.Deliverer
	S3        *s3.Client // platform bucket, for manifest upload
	Logger    *slog.Logger
	Interval  time.Duration
}

// New builds a Service with defaults.
func New(database *db.DB, deliverer *delivery.Deliverer, s3c *s3.Client, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{DB: database, Deliverer: deliverer, S3: s3c, Logger: logger, Interval: defaultInterval}
}

// Run loops until ctx is cancelled.
func (s *Service) Run(ctx context.Context) error {
	interval := s.Interval
	if interval <= 0 {
		interval = defaultInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	s.Logger.Info("batching.started", "interval", interval.String())
	for {
		select {
		case <-ctx.Done():
			s.Logger.Info("batching.stopped")
			return nil
		case <-t.C:
			if err := s.PushPendingResults(ctx); err != nil {
				s.Logger.Error("batching.push_failed", "err", err)
			}
			if err := s.AggregateBatches(ctx); err != nil {
				s.Logger.Error("batching.aggregate_failed", "err", err)
			}
		}
	}
}

type pendingPush struct {
	jobID  string
	result []byte
	dest   delivery.Destination
}

// PushPendingResults delivers result.json for completed batched jobs whose
// batch has a destination and which have not yet been pushed.
func (s *Service) PushPendingResults(ctx context.Context) error {
	if s.DB == nil || s.Deliverer == nil {
		return nil
	}
	var pending []pendingPush
	if err := s.withServiceTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT j.id::text, COALESCE(j.result, '{}'::jsonb),
			       d.type, d.bucket, COALESCE(d.prefix,''), COALESCE(d.region,''),
			       COALESCE(d.role_arn,''), COALESCE(d.external_id,''), COALESCE(d.endpoint,'')
			FROM jobs j
			JOIN batches b ON j.batch_id = b.id
			JOIN delivery_destinations d ON b.destination_id = d.id
			WHERE j.status = 'completed' AND j.delivery_status IS NULL
			ORDER BY j.completed_at ASC
			LIMIT $1
		`, pushBatch)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var p pendingPush
			if err := rows.Scan(&p.jobID, &p.result, &p.dest.Type, &p.dest.Bucket, &p.dest.Prefix,
				&p.dest.Region, &p.dest.RoleARN, &p.dest.ExternalID, &p.dest.Endpoint); err != nil {
				return err
			}
			pending = append(pending, p)
		}
		return rows.Err()
	}); err != nil {
		return err
	}

	for _, p := range pending {
		status := "delivered"
		if err := s.Deliverer.Push(ctx, p.dest, p.jobID+"/result.json", p.result, "application/json"); err != nil {
			s.Logger.Warn("batching.push_result_failed", "job_id", p.jobID, "err", err)
			status = "delivery_failed"
		}
		_ = s.withServiceTx(ctx, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE jobs SET delivery_status = $1 WHERE id = $2`, status, p.jobID)
			return err
		})
	}
	return nil
}

// AggregateBatches recomputes each running batch's counts and finalizes those
// whose children are all terminal (uploading a manifest + firing the callback).
func (s *Service) AggregateBatches(ctx context.Context) error {
	if s.DB == nil {
		return nil
	}
	type batchState struct {
		id, orgID, name, callbackWebhook      string
		jobCount, completed, failed, terminal int
	}
	var batches []batchState
	if err := s.withServiceTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT b.id::text, b.org_id::text, b.name, COALESCE(b.callback_webhook_id::text,''),
			       b.job_count,
			       (SELECT count(*) FROM jobs WHERE batch_id = b.id AND status = 'completed')::int,
			       (SELECT count(*) FROM jobs WHERE batch_id = b.id AND status IN ('failed','dead_letter'))::int,
			       (SELECT count(*) FROM jobs WHERE batch_id = b.id AND status IN ('completed','failed','dead_letter','canceled'))::int
			FROM batches b
			WHERE b.status = 'running'
		`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var b batchState
			if err := rows.Scan(&b.id, &b.orgID, &b.name, &b.callbackWebhook, &b.jobCount, &b.completed, &b.failed, &b.terminal); err != nil {
				return err
			}
			batches = append(batches, b)
		}
		return rows.Err()
	}); err != nil {
		return err
	}

	for _, b := range batches {
		// Refresh counts.
		if err := s.withServiceTx(ctx, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE batches SET completed_count=$1, failed_count=$2, updated_at=now() WHERE id=$3`,
				b.completed, b.failed, b.id)
			return err
		}); err != nil {
			s.Logger.Error("batching.count_update_failed", "batch_id", b.id, "err", err)
			continue
		}
		if b.jobCount == 0 || b.terminal < b.jobCount {
			continue // not all terminal yet
		}
		s.finalize(ctx, b.id, b.orgID, b.name, b.callbackWebhook, b.jobCount, b.completed, b.failed)
	}
	return nil
}

func (s *Service) finalize(ctx context.Context, batchID, orgID, name, callbackWebhook string, jobCount, completed, failed int) {
	status := "completed"
	if failed > 0 {
		status = "failed"
	}

	// Build + upload the manifest (best-effort).
	manifestKey := ""
	if s.S3 != nil {
		if jobs, err := s.manifestJobs(ctx, batchID); err == nil {
			manifest, _ := json.Marshal(map[string]any{
				"batch_id": batchID, "name": name, "status": status,
				"counts": map[string]int{"total": jobCount, "completed": completed, "failed": failed},
				"jobs":   jobs,
			})
			key := "batches/" + orgID + "/" + batchID + "/manifest.json"
			ct := "application/json"
			bkt := s.S3.Bucket()
			if _, err := s.S3.Raw().PutObject(ctx, &awss3.PutObjectInput{
				Bucket: &bkt, Key: &key, Body: bytes.NewReader(manifest), ContentType: &ct,
			}); err == nil {
				manifestKey = key
			} else {
				s.Logger.Warn("batching.manifest_upload_failed", "batch_id", batchID, "err", err)
			}
		}
	}

	// Finalize the batch row + fire the callback exactly once.
	_ = s.withServiceTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			UPDATE batches SET status=$1, manifest_s3_key=NULLIF($2,''), updated_at=now() WHERE id=$3
		`, status, manifestKey, batchID); err != nil {
			return err
		}
		if callbackWebhook != "" {
			payload, _ := json.Marshal(map[string]any{
				"batch_id": batchID, "status": status,
				"counts":          map[string]int{"total": jobCount, "completed": completed, "failed": failed},
				"manifest_s3_key": manifestKey,
			})
			// Insert a targeted delivery (the callback endpoint may not be
			// subscribed to batch.completed; the callback is explicit).
			if _, err := tx.Exec(ctx, `
				INSERT INTO webhook_deliveries
				  (id, org_id, endpoint_id, event_type, event_id, payload, status, next_retry_at, attempt_count, max_attempts, created_at)
				SELECT gen_random_uuid(), $1, $2::uuid, 'batch.completed', gen_random_uuid(), $3::jsonb, 'pending', now(), 0, 24, now()
				WHERE EXISTS (SELECT 1 FROM webhook_endpoints WHERE id=$2::uuid AND org_id=$1)
				  AND NOT (SELECT callback_sent FROM batches WHERE id=$4)
			`, orgID, callbackWebhook, payload, batchID); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, `UPDATE batches SET callback_sent=true WHERE id=$1`, batchID)
		return err
	})
	s.Logger.Info("batching.finalized", "batch_id", batchID, "status", status)
}

func (s *Service) manifestJobs(ctx context.Context, batchID string) ([]map[string]any, error) {
	var out []map[string]any
	err := s.withServiceTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id::text, status::text, COALESCE(delivery_status,'')
			FROM jobs WHERE batch_id=$1 ORDER BY created_at
		`, batchID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id, st, ds string
			if err := rows.Scan(&id, &st, &ds); err != nil {
				return err
			}
			m := map[string]any{"job_id": id, "status": st}
			if ds != "" {
				m["delivery_status"] = ds
			}
			out = append(out, m)
		}
		return rows.Err()
	})
	return out, err
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
