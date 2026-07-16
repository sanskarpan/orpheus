package retention

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/orpheus/api/internal/db"
)

type stubAborter struct {
	mu    sync.Mutex
	calls [][2]string // (key, uploadID)
}

func (s *stubAborter) AbortMultipartUpload(_ context.Context, key, uploadID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, [2]string{key, uploadID})
	return nil
}

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

// TestSweepOnce expires an old pending upload session (aborting its S3
// multipart) and deletes an expired idempotency key, and leaves fresh
// rows untouched.
func TestSweepOnce(t *testing.T) {
	dsn := os.Getenv("ORPHEUS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ORPHEUS_TEST_DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	pool, err := db.New(ctx, dsn) // plain pool (no is_service) — sweeper sets it itself
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	t.Cleanup(pool.Close)
	svc := servicePool(t, dsn)

	orgID := uuid.NewString()
	if _, err := svc.Exec(ctx, `INSERT INTO organizations (id,name,slug) VALUES ($1,$2,$2)`, orgID, "ret-"+orgID); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	t.Cleanup(func() {
		cctx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		_, _ = svc.Exec(cctx, `DELETE FROM idempotency_keys WHERE org_id=$1`, orgID)
		_, _ = svc.Exec(cctx, `DELETE FROM upload_sessions WHERE org_id=$1`, orgID)
		_, _ = svc.Exec(cctx, `DELETE FROM organizations WHERE id=$1`, orgID)
	})

	// Expired pending session (with S3 coords) + a fresh pending session.
	expiredID, freshID := uuid.NewString(), uuid.NewString()
	if _, err := svc.Exec(ctx, `
		INSERT INTO upload_sessions (id,org_id,filename,content_type,size_bytes,status,expires_at,s3_bucket,s3_key,s3_upload_id)
		VALUES ($1,$2,'a.wav','audio/wav',10,'pending', now() - interval '1 hour','b',$4,$5),
		       ($3,$2,'b.wav','audio/wav',10,'pending', now() + interval '1 hour','b',$6,$7)
	`, expiredID, orgID, freshID, "k/"+expiredID, "up-"+expiredID, "k/"+freshID, "up-"+freshID); err != nil {
		t.Fatalf("seed sessions: %v", err)
	}
	// Expired + fresh idempotency keys.
	if _, err := svc.Exec(ctx, `
		INSERT INTO idempotency_keys (id,org_id,key,request_hash,status,expires_at)
		VALUES (gen_random_uuid(),$1,'old','h','completed', now() - interval '1 hour'),
		       (gen_random_uuid(),$1,'new','h','completed', now() + interval '1 hour')
	`, orgID); err != nil {
		t.Fatalf("seed idem keys: %v", err)
	}

	aborter := &stubAborter{}
	s := &Sweeper{DB: pool, S3: aborter, Batch: 100}
	res, err := s.SweepOnce(ctx)
	if err != nil {
		t.Fatalf("SweepOnce: %v", err)
	}
	if res.ExpiredSessions < 1 {
		t.Errorf("ExpiredSessions = %d, want >= 1", res.ExpiredSessions)
	}
	if res.DeletedIdemKeys < 1 {
		t.Errorf("DeletedIdemKeys = %d, want >= 1", res.DeletedIdemKeys)
	}

	// The expired session is now 'expired'; the fresh one is still pending.
	var expiredStatus, freshStatus string
	_ = svc.QueryRow(ctx, `SELECT status FROM upload_sessions WHERE id=$1`, expiredID).Scan(&expiredStatus)
	_ = svc.QueryRow(ctx, `SELECT status FROM upload_sessions WHERE id=$1`, freshID).Scan(&freshStatus)
	if expiredStatus != "expired" {
		t.Errorf("expired session status = %q, want expired", expiredStatus)
	}
	if freshStatus != "pending" {
		t.Errorf("fresh session status = %q, want pending", freshStatus)
	}

	// The multipart abort was called for the expired session.
	aborter.mu.Lock()
	calls := len(aborter.calls)
	aborter.mu.Unlock()
	if calls < 1 {
		t.Errorf("AbortMultipartUpload called %d times, want >= 1", calls)
	}

	// The expired idempotency key is gone; the fresh one remains.
	var oldCnt, newCnt int
	_ = svc.QueryRow(ctx, `SELECT count(*) FROM idempotency_keys WHERE org_id=$1 AND key='old'`, orgID).Scan(&oldCnt)
	_ = svc.QueryRow(ctx, `SELECT count(*) FROM idempotency_keys WHERE org_id=$1 AND key='new'`, orgID).Scan(&newCnt)
	if oldCnt != 0 || newCnt != 1 {
		t.Errorf("idempotency keys: old=%d new=%d, want 0/1", oldCnt, newCnt)
	}
}
