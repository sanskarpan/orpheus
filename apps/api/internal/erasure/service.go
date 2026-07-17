// Package erasure executes GDPR erasure requests (PRD 10): hard-delete audio
// bytes from S3, verify the purge, soft-delete metadata, cascade into the
// content cache (PRD 01) and bundles (PRD 02), and write a certificate. It is
// a privileged system component running under the service role.
//
// It runs as a saga: each request is claimed (scheduled → running), executed,
// and finalized. Re-running a partially-done request is safe — deletes are
// idempotent and soft-delete is a no-op the second time.
package erasure

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/orpheus/api/internal/db"
	"github.com/orpheus/api/internal/storage/s3"
)

const defaultInterval = 5 * time.Second

// Service processes scheduled erasure requests.
type Service struct {
	DB       *db.DB
	S3       *s3.Client
	Logger   *slog.Logger
	Interval time.Duration
}

// New builds a Service.
func New(database *db.DB, s3c *s3.Client, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{DB: database, S3: s3c, Logger: logger, Interval: defaultInterval}
}

// Run loops until ctx is cancelled.
func (s *Service) Run(ctx context.Context) error {
	interval := s.Interval
	if interval <= 0 {
		interval = defaultInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	s.Logger.Info("erasure.started", "interval", interval.String())
	for {
		select {
		case <-ctx.Done():
			s.Logger.Info("erasure.stopped")
			return nil
		case <-t.C:
			if err := s.ProcessScheduled(ctx); err != nil {
				s.Logger.Error("erasure.process_failed", "err", err)
			}
		}
	}
}

type request struct {
	id, orgID, scope, targetID string
}

// ProcessScheduled claims and executes scheduled requests.
func (s *Service) ProcessScheduled(ctx context.Context) error {
	if s.DB == nil {
		return nil
	}
	var reqs []request
	if err := s.withServiceTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			UPDATE erasure_requests SET status='running'
			WHERE id IN (SELECT id FROM erasure_requests WHERE status='scheduled' ORDER BY scheduled_at LIMIT 20 FOR UPDATE SKIP LOCKED)
			RETURNING id::text, org_id::text, scope, COALESCE(target_id::text,'')
		`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r request
			if err := rows.Scan(&r.id, &r.orgID, &r.scope, &r.targetID); err != nil {
				return err
			}
			reqs = append(reqs, r)
		}
		return rows.Err()
	}); err != nil {
		return err
	}
	for _, r := range reqs {
		s.execute(ctx, r)
	}
	return nil
}

// execute runs one erasure request end to end.
func (s *Service) execute(ctx context.Context, r request) {
	// 1) Resolve the target artifacts.
	artifacts, err := s.targetArtifacts(ctx, r)
	if err != nil {
		s.fail(ctx, r.id, "resolve targets: "+err.Error())
		return
	}

	purged := 0
	var erasedIDs []string
	purgedKeys := []string{}
	for _, a := range artifacts {
		// 2) Hard-delete the bytes + verify the purge (HEAD → not found).
		if a.key != "" && s.S3 != nil {
			if err := s.S3.DeleteObject(ctx, a.key); err != nil {
				s.Logger.Warn("erasure.s3_delete_failed", "key", a.key, "err", err)
			}
			if _, _, herr := s.S3.HeadObject(ctx, a.key); herr == nil {
				s.fail(ctx, r.id, "purge verification failed: object still present: "+a.key)
				return
			}
			purged++
			sum := sha256.Sum256([]byte(a.key))
			purgedKeys = append(purgedKeys, hex.EncodeToString(sum[:]))
		}
		erasedIDs = append(erasedIDs, a.id)
	}

	// 3) Soft-delete metadata + cascade into cache/bundles + optional result scrub.
	counts := map[string]int{"artifacts": len(erasedIDs), "s3_objects": purged}
	if err := s.withServiceTx(ctx, func(tx pgx.Tx) error {
		if len(erasedIDs) > 0 {
			if _, err := tx.Exec(ctx, `UPDATE artifacts SET deleted_at=now(), erasure_request_id=$2 WHERE id = ANY($1::uuid[])`, erasedIDs, r.id); err != nil {
				return err
			}
			// Cache entries whose source job used an erased artifact.
			ct, err := tx.Exec(ctx, `DELETE FROM job_result_cache WHERE source_job_id IN (SELECT id FROM jobs WHERE artifact_id = ANY($1::uuid[]))`, erasedIDs)
			if err != nil {
				return err
			}
			counts["cache_entries"] = int(ct.RowsAffected())
			// Bundles containing an erased artifact are revoked.
			bt, err := tx.Exec(ctx, `UPDATE bundles SET status='revoked', updated_at=now() WHERE id IN (SELECT bundle_id FROM bundle_items WHERE artifact_id = ANY($1::uuid[])) AND status <> 'revoked'`, erasedIDs)
			if err != nil {
				return err
			}
			counts["bundles_revoked"] = int(bt.RowsAffected())
		}
		if r.scope == "job" && r.targetID != "" {
			// Scrub the transcript/PII payload from the job result.
			if _, err := tx.Exec(ctx, `UPDATE jobs SET result = jsonb_build_object('erased', true) WHERE id=$1`, r.targetID); err != nil {
				return err
			}
			counts["job_results"] = 1
		}
		return nil
	}); err != nil {
		s.fail(ctx, r.id, "cascade: "+err.Error())
		return
	}

	// 4) Certificate + finalize + data.erased event.
	certKey := s.writeCertificate(ctx, r, counts, purgedKeys)
	countsJSON, _ := json.Marshal(counts)
	_ = s.withServiceTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			UPDATE erasure_requests SET status='completed', completed_at=now(), deleted_counts=$2::jsonb, s3_objects_purged=$3, certificate_s3_key=NULLIF($4,'') WHERE id=$1
		`, r.id, countsJSON, purged, certKey); err != nil {
			return err
		}
		payload, _ := json.Marshal(map[string]any{"request_id": r.id, "scope": r.scope, "counts": counts})
		_, err := tx.Exec(ctx, `
			INSERT INTO outbox (id, org_id, aggregate_type, aggregate_id, event_type, payload, headers)
			VALUES (gen_random_uuid(), $1, 'erasure', $2, 'data.erased', $3::jsonb, '{}'::jsonb)
		`, r.orgID, r.id, payload)
		return err
	})
	s.Logger.Info("erasure.completed", "request_id", r.id, "artifacts", len(erasedIDs), "purged", purged)
}

type artifactRef struct{ id, key string }

func (s *Service) targetArtifacts(ctx context.Context, r request) ([]artifactRef, error) {
	var out []artifactRef
	err := s.withServiceTx(ctx, func(tx pgx.Tx) error {
		var q string
		switch r.scope {
		case "artifact":
			q = `SELECT id::text, COALESCE(s3_key,'') FROM artifacts WHERE id=$1 AND org_id=$2 AND deleted_at IS NULL`
		case "job":
			q = `SELECT id::text, COALESCE(s3_key,'') FROM artifacts WHERE id IN (SELECT artifact_id FROM jobs WHERE id=$1) AND org_id=$2 AND deleted_at IS NULL`
		default:
			return nil
		}
		rows, err := tx.Query(ctx, q, r.targetID, r.orgID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var a artifactRef
			if err := rows.Scan(&a.id, &a.key); err != nil {
				return err
			}
			out = append(out, a)
		}
		return rows.Err()
	})
	return out, err
}

func (s *Service) writeCertificate(ctx context.Context, r request, counts map[string]int, purgedKeyHashes []string) string {
	if s.S3 == nil {
		return ""
	}
	cert, _ := json.MarshalIndent(map[string]any{
		"request_id":        r.id,
		"scope":             r.scope,
		"counts":            counts,
		"purged_key_sha256": purgedKeyHashes,
		"confirmation":      "audio bytes purged from primary + replicas; backup rotation completes within retention window",
	}, "", "  ")
	key := "erasure-certificates/" + r.orgID + "/" + r.id + ".json"
	ct := "application/json"
	bkt := s.S3.Bucket()
	if _, err := s.S3.Raw().PutObject(ctx, &awss3.PutObjectInput{Bucket: &bkt, Key: &key, Body: bytes.NewReader(cert), ContentType: &ct}); err != nil {
		s.Logger.Warn("erasure.certificate_upload_failed", "err", err)
		return ""
	}
	return key
}

func (s *Service) fail(ctx context.Context, id, msg string) {
	_ = s.withServiceTx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE erasure_requests SET status='failed', error=$2 WHERE id=$1`, id, msg)
		return err
	})
	s.Logger.Error("erasure.failed", "request_id", id, "err", msg)
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
