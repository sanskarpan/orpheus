package batching

import (
	"context"
	"encoding/json"
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
	"github.com/orpheus/api/internal/delivery"
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

// TestBatching_PushAndFinalize drives the batching service against live
// Postgres + MinIO: two completed child jobs get their result.json pushed to
// a static (MinIO) destination, then the batch finalizes with a manifest and a
// targeted callback delivery.
func TestBatching_PushAndFinalize(t *testing.T) {
	dsn := os.Getenv("ORPHEUS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ORPHEUS_TEST_DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	svc := servicePool(t, dsn)
	cfg := &config.Config{
		S3Endpoint: "http://127.0.0.1:9000", S3AccessKey: "orpheus",
		S3SecretKey: "orpheus-dev-secret", S3Bucket: "orpheus-uploads",
	}
	s3c, err := s3.New(ctx, cfg)
	if err != nil {
		t.Skipf("s3 unavailable: %v", err)
	}
	deliverer := &delivery.Deliverer{
		StaticEndpoint: cfg.S3Endpoint, StaticAccessKey: cfg.S3AccessKey, StaticSecretKey: cfg.S3SecretKey,
	}
	service := New(svc, deliverer, s3c, slog.New(slog.NewTextHandler(io.Discard, nil)))

	orgID := uuid.NewString()
	if _, err := svc.Exec(ctx, `INSERT INTO organizations (id,name,slug) VALUES ($1,$2,$2)`, orgID, "bat-"+orgID); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	epID := uuid.NewString()
	if _, err := svc.Exec(ctx, `INSERT INTO webhook_endpoints (id,org_id,url,secret,subscribed_events,active) VALUES ($1,$2,'https://example.com/cb','s',ARRAY['batch.completed'],true)`, epID, orgID); err != nil {
		t.Fatalf("seed endpoint: %v", err)
	}
	prefix := "tenant-delivery/" + orgID + "/"
	destID := uuid.NewString()
	if _, err := svc.Exec(ctx, `INSERT INTO delivery_destinations (id,org_id,type,bucket,prefix,region,endpoint) VALUES ($1,$2,'s3_static',$3,$4,'us-east-1',$5)`,
		destID, orgID, "orpheus-uploads", prefix, cfg.S3Endpoint); err != nil {
		t.Fatalf("seed destination: %v", err)
	}
	batchID := uuid.NewString()
	if _, err := svc.Exec(ctx, `INSERT INTO batches (id,org_id,name,status,job_count,callback_webhook_id,destination_id) VALUES ($1,$2,'nightly','running',2,$3,$4)`,
		batchID, orgID, epID, destID); err != nil {
		t.Fatalf("seed batch: %v", err)
	}
	var jobIDs []string
	for i := 0; i < 2; i++ {
		jid := uuid.NewString()
		jobIDs = append(jobIDs, jid)
		if _, err := svc.Exec(ctx, `INSERT INTO jobs (id,org_id,batch_id,job_type,status,result,completed_at) VALUES ($1,$2,$3,'transcribe'::job_type,'completed'::job_status,$4::jsonb,now())`,
			jid, orgID, batchID, `{"text":"hello","n":`+string(rune('0'+i))+`}`); err != nil {
			t.Fatalf("seed job: %v", err)
		}
	}
	t.Cleanup(func() {
		c, cc := context.WithTimeout(context.Background(), 5*time.Second)
		defer cc()
		_, _ = svc.Exec(c, `DELETE FROM webhook_deliveries WHERE org_id=$1`, orgID)
		_, _ = svc.Exec(c, `DELETE FROM jobs WHERE org_id=$1`, orgID)
		_, _ = svc.Exec(c, `DELETE FROM batches WHERE org_id=$1`, orgID)
		_, _ = svc.Exec(c, `DELETE FROM delivery_destinations WHERE org_id=$1`, orgID)
		_, _ = svc.Exec(c, `DELETE FROM webhook_endpoints WHERE org_id=$1`, orgID)
		_, _ = svc.Exec(c, `DELETE FROM organizations WHERE id=$1`, orgID)
	})

	// 1) Push results → both objects land in MinIO + delivery_status set.
	if err := service.PushPendingResults(ctx); err != nil {
		t.Fatalf("push: %v", err)
	}
	for _, jid := range jobIDs {
		key := prefix + jid + "/result.json"
		if _, err := s3c.Raw().HeadObject(ctx, &awss3.HeadObjectInput{Bucket: strptr("orpheus-uploads"), Key: &key}); err != nil {
			t.Fatalf("result object %s missing: %v", key, err)
		}
	}
	var deliveredCount int
	if err := svc.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE batch_id=$1 AND delivery_status='delivered'`, batchID).Scan(&deliveredCount); err != nil {
		t.Fatalf("count delivered: %v", err)
	}
	if deliveredCount != 2 {
		t.Fatalf("delivered=%d, want 2", deliveredCount)
	}

	// 2) Aggregate → batch completed, manifest uploaded, callback delivery queued.
	if err := service.AggregateBatches(ctx); err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	var status, manifestKey string
	if err := svc.QueryRow(ctx, `SELECT status, COALESCE(manifest_s3_key,'') FROM batches WHERE id=$1`, batchID).Scan(&status, &manifestKey); err != nil {
		t.Fatalf("read batch: %v", err)
	}
	if status != "completed" || manifestKey == "" {
		t.Fatalf("batch status=%q manifest=%q, want completed + manifest", status, manifestKey)
	}
	body, err := s3c.Raw().GetObject(ctx, &awss3.GetObjectInput{Bucket: strptr("orpheus-uploads"), Key: &manifestKey})
	if err != nil {
		t.Fatalf("manifest object missing: %v", err)
	}
	mb, _ := io.ReadAll(body.Body)
	var manifest map[string]any
	_ = json.Unmarshal(mb, &manifest)
	if manifest["status"] != "completed" {
		t.Fatalf("manifest status = %v", manifest["status"])
	}
	var callbackCount int
	if err := svc.QueryRow(ctx, `SELECT count(*) FROM webhook_deliveries WHERE org_id=$1 AND event_type='batch.completed'`, orgID).Scan(&callbackCount); err != nil {
		t.Fatalf("count callback: %v", err)
	}
	if callbackCount != 1 {
		t.Fatalf("callback deliveries=%d, want 1", callbackCount)
	}

	// 3) Idempotent: re-running does not double-fire the callback.
	if err := service.AggregateBatches(ctx); err != nil {
		t.Fatalf("aggregate 2: %v", err)
	}
	if err := svc.QueryRow(ctx, `SELECT count(*) FROM webhook_deliveries WHERE org_id=$1 AND event_type='batch.completed'`, orgID).Scan(&callbackCount); err != nil {
		t.Fatalf("recount callback: %v", err)
	}
	if callbackCount != 1 {
		t.Fatalf("callback re-fired: deliveries=%d, want 1", callbackCount)
	}
}

func strptr(s string) *string { return &s }
