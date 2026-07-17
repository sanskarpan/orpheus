package erasure

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/orpheus/api/internal/config"
	"github.com/orpheus/api/internal/db"
	"github.com/orpheus/api/internal/storage/s3"
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

func strptr(s string) *string { return &s }

// TestErasure_HardDeleteWithCascade seeds an artifact (bytes in MinIO) that a
// cache entry and a bundle reference, then runs the erasure saga and verifies:
// the S3 object is purged (HEAD 404), the artifact is soft-deleted, the cache
// entry is dropped, the bundle is revoked, a certificate is written, and a
// data.erased event is emitted.
func TestErasure_HardDeleteWithCascade(t *testing.T) {
	dsn := os.Getenv("ORPHEUS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ORPHEUS_TEST_DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	svc := servicePool(t, dsn)
	cfg := &config.Config{S3Endpoint: "http://127.0.0.1:9000", S3AccessKey: "orpheus", S3SecretKey: "orpheus-dev-secret", S3Bucket: "orpheus-uploads"}
	s3c, err := s3.New(ctx, cfg)
	if err != nil {
		t.Skipf("s3 unavailable: %v", err)
	}
	service := New(svc, s3c, slog.New(slog.NewTextHandler(io.Discard, nil)))

	orgID := uuid.NewString()
	if _, err := svc.Exec(ctx, `INSERT INTO organizations (id,name,slug) VALUES ($1,$2,$2)`, orgID, "er-"+orgID); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	// Object in MinIO + artifact row.
	key := "uploads/" + orgID + "/erase-me.wav"
	ct := "audio/wav"
	bkt := "orpheus-uploads"
	if _, err := s3c.Raw().PutObject(ctx, &awss3.PutObjectInput{Bucket: &bkt, Key: &key, Body: bytes.NewReader([]byte("audio")), ContentType: &ct}); err != nil {
		t.Fatalf("put object: %v", err)
	}
	artID := uuid.NewString()
	if _, err := svc.Exec(ctx, `INSERT INTO artifacts (id,org_id,s3_bucket,s3_key,sha256,size_bytes,content_type) VALUES ($1,$2,'orpheus-uploads',$3,$4,5,'audio/wav')`, artID, orgID, key, "sha-e"); err != nil {
		t.Fatalf("seed artifact: %v", err)
	}
	// A completed job on the artifact + a cache entry from it.
	jobID := uuid.NewString()
	_, _ = svc.Exec(ctx, `INSERT INTO jobs (id,org_id,artifact_id,job_type,status,result) VALUES ($1,$2,$3,'transcribe'::job_type,'completed'::job_status,'{"text":"x"}'::jsonb)`, jobID, orgID, artID)
	_, _ = svc.Exec(ctx, `INSERT INTO job_result_cache (org_id,cache_key,input_hash,params_hash,model_version_id,source_job_id,result) VALUES ($1,'\x01','ih','ph','mv',$2,'{}'::jsonb)`, orgID, jobID)
	// A bundle containing the artifact.
	bundleID := uuid.NewString()
	_, _ = svc.Exec(ctx, `INSERT INTO bundles (id,org_id,name,status,result_docs) VALUES ($1,$2,'b','ready','{}'::jsonb)`, bundleID, orgID)
	_, _ = svc.Exec(ctx, `INSERT INTO bundle_items (bundle_id,org_id,artifact_id,path_in_zip) VALUES ($1,$2,$3,'a.wav')`, bundleID, orgID, artID)
	// The erasure request.
	reqID := uuid.NewString()
	if _, err := svc.Exec(ctx, `INSERT INTO erasure_requests (id,org_id,scope,target_id,reason,status) VALUES ($1,$2,'artifact',$3,'gdpr_art17','scheduled')`, reqID, orgID, artID); err != nil {
		t.Fatalf("seed request: %v", err)
	}
	t.Cleanup(func() {
		c, cc := context.WithTimeout(context.Background(), 5*time.Second)
		defer cc()
		_, _ = svc.Exec(c, `DELETE FROM job_result_cache WHERE org_id=$1`, orgID)
		_, _ = svc.Exec(c, `DELETE FROM bundle_items WHERE org_id=$1`, orgID)
		_, _ = svc.Exec(c, `DELETE FROM bundles WHERE org_id=$1`, orgID)
		_, _ = svc.Exec(c, `DELETE FROM erasure_requests WHERE org_id=$1`, orgID)
		_, _ = svc.Exec(c, `DELETE FROM outbox WHERE org_id=$1`, orgID)
		_, _ = svc.Exec(c, `DELETE FROM jobs WHERE org_id=$1`, orgID)
		_, _ = svc.Exec(c, `DELETE FROM artifacts WHERE org_id=$1`, orgID)
		_, _ = svc.Exec(c, `DELETE FROM organizations WHERE id=$1`, orgID)
	})

	if err := service.ProcessScheduled(ctx); err != nil {
		t.Fatalf("process: %v", err)
	}

	// Request completed with a certificate.
	var status, certKey string
	var purged int
	if err := svc.QueryRow(ctx, `SELECT status, s3_objects_purged, COALESCE(certificate_s3_key,'') FROM erasure_requests WHERE id=$1`, reqID).Scan(&status, &purged, &certKey); err != nil {
		t.Fatalf("read request: %v", err)
	}
	if status != "completed" || purged != 1 || certKey == "" {
		t.Fatalf("request = %s purged %d cert %q, want completed/1/cert", status, purged, certKey)
	}
	// S3 object purged (verify HEAD → error).
	if _, _, err := s3c.HeadObject(ctx, key); err == nil {
		t.Fatal("object still present after erasure")
	}
	// Certificate object exists.
	if _, err := s3c.Raw().HeadObject(ctx, &awss3.HeadObjectInput{Bucket: strptr("orpheus-uploads"), Key: &certKey}); err != nil {
		t.Fatalf("certificate missing: %v", err)
	}
	// Artifact soft-deleted; cache dropped; bundle revoked; event emitted.
	var deletedAt *time.Time
	_ = svc.QueryRow(ctx, `SELECT deleted_at FROM artifacts WHERE id=$1`, artID).Scan(&deletedAt)
	if deletedAt == nil {
		t.Fatal("artifact not soft-deleted")
	}
	var cacheCount, bundleRevoked, eventCount int
	_ = svc.QueryRow(ctx, `SELECT count(*) FROM job_result_cache WHERE org_id=$1`, orgID).Scan(&cacheCount)
	_ = svc.QueryRow(ctx, `SELECT count(*) FROM bundles WHERE id=$1 AND status='revoked'`, bundleID).Scan(&bundleRevoked)
	_ = svc.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE org_id=$1 AND event_type='data.erased'`, orgID).Scan(&eventCount)
	if cacheCount != 0 || bundleRevoked != 1 || eventCount != 1 {
		t.Fatalf("cascade: cache=%d bundleRevoked=%d event=%d, want 0/1/1", cacheCount, bundleRevoked, eventCount)
	}
}
