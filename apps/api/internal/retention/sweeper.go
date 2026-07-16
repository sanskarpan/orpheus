// Package retention runs a background sweeper that expires abandoned
// state: pending upload sessions past their deadline (aborting the S3
// multipart upload so we don't pay for dangling parts) and idempotency
// keys past their TTL. It is a system component and runs with the service
// role, in transaction-local scope, so it can act across every org's
// FORCE-RLS rows without a request principal.
package retention

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/orpheus/api/internal/db"
)

const (
	defaultInterval = time.Hour
	defaultBatch    = 500
)

// MultipartAborter aborts an in-progress S3 multipart upload. *s3.Client
// satisfies it; tests supply a stub. A nil aborter skips the S3 step.
type MultipartAborter interface {
	AbortMultipartUpload(ctx context.Context, key, uploadID string) error
}

// Sweeper periodically expires abandoned upload sessions and idempotency
// keys. Safe for concurrent instances (claims use FOR UPDATE SKIP LOCKED).
type Sweeper struct {
	DB       *db.DB
	S3       MultipartAborter
	Logger   *slog.Logger
	Interval time.Duration
	Batch    int
}

// New constructs a Sweeper with sensible defaults.
func New(database *db.DB, aborter MultipartAborter, logger *slog.Logger) *Sweeper {
	if logger == nil {
		logger = slog.Default()
	}
	return &Sweeper{DB: database, S3: aborter, Logger: logger, Interval: defaultInterval, Batch: defaultBatch}
}

// SweepResult reports what a single sweep removed.
type SweepResult struct {
	ExpiredSessions int
	DeletedIdemKeys int
}

// Run loops until ctx is cancelled, sweeping every Interval.
func (s *Sweeper) Run(ctx context.Context) error {
	interval := s.Interval
	if interval <= 0 {
		interval = defaultInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	s.Logger.Info("retention.sweeper.started", "interval", interval.String())
	for {
		select {
		case <-ctx.Done():
			s.Logger.Info("retention.sweeper.stopped")
			return nil
		case <-t.C:
			if res, err := s.SweepOnce(ctx); err != nil {
				s.Logger.Error("retention.sweep_failed", "err", err)
			} else if res.ExpiredSessions > 0 || res.DeletedIdemKeys > 0 {
				s.Logger.Info("retention.swept", "expired_sessions", res.ExpiredSessions, "deleted_idempotency_keys", res.DeletedIdemKeys)
			}
		}
	}
}

// SweepOnce runs one sweep and returns what it removed. Exported for tests
// and for an on-demand admin trigger.
func (s *Sweeper) SweepOnce(ctx context.Context) (SweepResult, error) {
	var res SweepResult
	if s.DB == nil {
		return res, nil
	}

	// 1) Claim expired pending sessions, mark them 'expired', return their
	//    S3 coordinates. Marking inside the tx (not just selecting) is what
	//    stops a concurrent sweeper from double-aborting the same upload.
	type expiredSession struct{ key, uploadID string }
	var sessions []expiredSession
	if err := s.withServiceTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			UPDATE upload_sessions
			SET status = 'expired'
			WHERE id IN (
				SELECT id FROM upload_sessions
				WHERE status = 'pending' AND expires_at < now()
				ORDER BY expires_at ASC
				LIMIT $1
				FOR UPDATE SKIP LOCKED
			)
			RETURNING COALESCE(s3_key, ''), COALESCE(s3_upload_id, '')
		`, s.Batch)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e expiredSession
			if err := rows.Scan(&e.key, &e.uploadID); err != nil {
				return err
			}
			sessions = append(sessions, e)
		}
		return rows.Err()
	}); err != nil {
		return res, err
	}
	res.ExpiredSessions = len(sessions)

	// 2) Abort the S3 multipart uploads OUTSIDE any DB transaction (network
	//    I/O). Best-effort: a failed abort is logged, not fatal — the parts
	//    also expire via the bucket lifecycle policy.
	if s.S3 != nil {
		for _, e := range sessions {
			if e.key == "" || e.uploadID == "" {
				continue
			}
			if err := s.S3.AbortMultipartUpload(ctx, e.key, e.uploadID); err != nil {
				s.Logger.Warn("retention.abort_multipart_failed", "err", err, "key", e.key)
			}
		}
	}

	// 3) Delete idempotency keys past their TTL.
	if err := s.withServiceTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM idempotency_keys WHERE expires_at < now()`)
		if err != nil {
			return err
		}
		res.DeletedIdemKeys = int(tag.RowsAffected())
		return nil
	}); err != nil {
		return res, err
	}

	return res, nil
}

// withServiceTx runs fn in a transaction with app.is_service set locally,
// so the sweeper can touch every org's FORCE-RLS rows.
func (s *Sweeper) withServiceTx(ctx context.Context, fn func(pgx.Tx) error) error {
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
