package webhooks

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/orpheus/api/internal/db"
	"github.com/orpheus/api/internal/ssrfguard"
)

// ── shared helpers ───────────────────────────────────────────────────

func webhookServicePool(t *testing.T, dsn string) *db.DB {
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

// seedEndpoint creates an org + active webhook endpoint (subscribed to
// job.queued) and returns (orgID, endpointID). Uses the service pool.
func seedEndpoint(t *testing.T, ctx context.Context, svc *db.DB, url, secret string) (string, string) {
	t.Helper()
	orgID := uuid.NewString()
	endpointID := uuid.NewString()
	if _, err := svc.Exec(ctx, `INSERT INTO organizations (id, name, slug) VALUES ($1, $2, $2)`, orgID, "wh-"+orgID); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = svc.Exec(cctx, `DELETE FROM webhook_deliveries WHERE org_id=$1`, orgID)
		_, _ = svc.Exec(cctx, `DELETE FROM webhook_endpoints WHERE org_id=$1`, orgID)
		_, _ = svc.Exec(cctx, `DELETE FROM audit_log WHERE org_id=$1`, orgID)
		_, _ = svc.Exec(cctx, `DELETE FROM organizations WHERE id=$1`, orgID)
	})
	if _, err := svc.Exec(ctx, `
		INSERT INTO webhook_endpoints (id, org_id, url, secret, subscribed_events, active)
		VALUES ($1, $2, $3, $4, ARRAY['job.queued'], true)
	`, endpointID, orgID, url, secret); err != nil {
		t.Fatalf("seed endpoint: %v", err)
	}
	return orgID, endpointID
}

func readDelivery(t *testing.T, ctx context.Context, svc *db.DB, orgID string) (status string, attempt int, nextRetry time.Time, respBody string) {
	t.Helper()
	var rb *string
	if err := svc.QueryRow(ctx, `
		SELECT status::text, attempt_count, next_retry_at, response_body
		FROM webhook_deliveries WHERE org_id=$1 ORDER BY created_at DESC LIMIT 1
	`, orgID).Scan(&status, &attempt, &nextRetry, &rb); err != nil {
		t.Fatalf("read delivery: %v", err)
	}
	if rb != nil {
		respBody = *rb
	}
	return
}

func newDelivery(pool *db.DB, client *http.Client) *DeliveryService {
	return &DeliveryService{
		DB:          pool,
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		HTTPClient:  client,
		Batch:       32,
		MaxAttempts: 24,
		Backoff:     defaultBackoff,
	}
}

func requireDSN(t *testing.T) string {
	dsn := os.Getenv("ORPHEUS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ORPHEUS_TEST_DATABASE_URL not set; skipping live webhook delivery test")
	}
	return dsn
}

// ── tests ────────────────────────────────────────────────────────────

// TestDelivery_EnqueueAndDeliverWithSignature is the end-to-end proof
// that (a) Enqueue actually creates delivery rows under RLS (ISSUE-24),
// (b) the delivery loop drains them under RLS (ISSUE-23), and (c) the
// receiver gets a correctly HMAC-signed request → row marked delivered.
func TestDelivery_EnqueueAndDeliverWithSignature(t *testing.T) {
	dsn := requireDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	pool, err := db.New(ctx, dsn)
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	t.Cleanup(pool.Close)
	svc := webhookServicePool(t, dsn)

	secret := "whsec_" + uuid.NewString()
	var gotValidSig atomic.Bool
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		body, _ := io.ReadAll(r.Body)
		// Parse "t=<ts>,v1=<sig>" and verify the HMAC.
		hdr := r.Header.Get(signatureHeader)
		var ts int64
		var sig string
		for _, part := range strings.Split(hdr, ",") {
			if strings.HasPrefix(part, "t=") {
				fmt.Sscanf(part, "t=%d", &ts)
			}
			if strings.HasPrefix(part, "v1=") {
				sig = strings.TrimPrefix(part, "v1=")
			}
		}
		if sig == signPayload(secret, ts, body) {
			gotValidSig.Store(true)
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	orgID, _ := seedEndpoint(t, ctx, svc, srv.URL, secret)

	d := newDelivery(pool, srv.Client())
	if err := d.Enqueue(ctx, orgID, "job.queued", uuid.NewString(), map[string]any{"job_id": "x"}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// Enqueue must have created a delivery row (RLS fix). Read as service.
	var pending int
	if err := svc.QueryRow(ctx, `SELECT count(*) FROM webhook_deliveries WHERE org_id=$1`, orgID).Scan(&pending); err != nil {
		t.Fatalf("count deliveries: %v", err)
	}
	if pending != 1 {
		t.Fatalf("Enqueue created %d delivery rows, want 1 (RLS drain?)", pending)
	}

	d.tick(ctx)

	if hits.Load() != 1 {
		t.Fatalf("receiver hit %d times, want 1", hits.Load())
	}
	if !gotValidSig.Load() {
		t.Fatal("receiver did not get a valid HMAC signature")
	}
	status, attempt, _, _ := readDelivery(t, ctx, svc, orgID)
	if status != "delivered" || attempt != 1 {
		t.Fatalf("delivery status=%q attempt=%d, want delivered/1", status, attempt)
	}
}

// TestDelivery_RetryOnServerError verifies a 5xx schedules a retry with a
// future next_retry_at (backoff) and is not immediately re-delivered.
func TestDelivery_RetryOnServerError(t *testing.T) {
	dsn := requireDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	pool, err := db.New(ctx, dsn)
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	t.Cleanup(pool.Close)
	svc := webhookServicePool(t, dsn)

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	orgID, _ := seedEndpoint(t, ctx, svc, srv.URL, "s3cr3t-abcdefghij")
	d := newDelivery(pool, srv.Client())
	if err := d.Enqueue(ctx, orgID, "job.queued", uuid.NewString(), map[string]any{}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	d.tick(ctx)
	if hits.Load() != 1 {
		t.Fatalf("receiver hit %d, want 1", hits.Load())
	}
	status, attempt, nextRetry, reason := readDelivery(t, ctx, svc, orgID)
	if status != "pending" || attempt != 1 {
		t.Fatalf("after 5xx: status=%q attempt=%d, want pending/1", status, attempt)
	}
	if !nextRetry.After(time.Now()) {
		t.Fatalf("next_retry_at %v is not in the future (backoff not scheduled)", nextRetry)
	}
	if !strings.Contains(reason, "500") {
		t.Errorf("failure reason = %q, want it to mention http 500", reason)
	}

	// A second tick must NOT re-deliver (next_retry_at is in the future).
	d.tick(ctx)
	if hits.Load() != 1 {
		t.Fatalf("re-delivered before backoff elapsed: hits=%d, want 1", hits.Load())
	}
}

// TestDelivery_SSRFBlockedAtDial verifies a delivery targeting an internal
// address is blocked by the SSRF-safe dialer and marked failed, without
// reaching any server.
func TestDelivery_SSRFBlockedAtDial(t *testing.T) {
	dsn := requireDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	pool, err := db.New(ctx, dsn)
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	t.Cleanup(pool.Close)
	svc := webhookServicePool(t, dsn)

	// Cloud metadata endpoint — must never be reachable.
	orgID, _ := seedEndpoint(t, ctx, svc, "https://169.254.169.254/latest/meta-data/", "s3cr3t-abcdefghij")
	// Use the real SSRF-safe client (not a test client).
	d := newDelivery(pool, ssrfguard.SafeHTTPClient(deliveryTimeout))
	if err := d.Enqueue(ctx, orgID, "job.queued", uuid.NewString(), map[string]any{}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	d.tick(ctx)
	status, attempt, _, reason := readDelivery(t, ctx, svc, orgID)
	if status == "delivered" {
		t.Fatal("SSRF target was delivered to — dialer did not block it")
	}
	if attempt != 1 {
		t.Errorf("attempt=%d, want 1", attempt)
	}
	if !strings.Contains(strings.ToLower(reason), "ssrf") && !strings.Contains(strings.ToLower(reason), "blocked") {
		t.Errorf("failure reason = %q, want it to indicate an SSRF/dial block", reason)
	}
}

// TestDelivery_ConcurrentClaimDeliversOnce verifies the atomic claim
// prevents two concurrent ticks from double-delivering one event.
func TestDelivery_ConcurrentClaimDeliversOnce(t *testing.T) {
	dsn := requireDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	pool, err := db.New(ctx, dsn)
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	t.Cleanup(pool.Close)
	svc := webhookServicePool(t, dsn)

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		time.Sleep(150 * time.Millisecond) // widen the race window
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	orgID, _ := seedEndpoint(t, ctx, svc, srv.URL, "s3cr3t-abcdefghij")
	d := newDelivery(pool, srv.Client())
	if err := d.Enqueue(ctx, orgID, "job.queued", uuid.NewString(), map[string]any{}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); d.tick(ctx) }()
	}
	wg.Wait()

	if got := hits.Load(); got != 1 {
		t.Fatalf("event delivered %d times under concurrency, want exactly 1", got)
	}
	status, attempt, _, _ := readDelivery(t, ctx, svc, orgID)
	if status != "delivered" || attempt != 1 {
		t.Fatalf("status=%q attempt=%d, want delivered/1", status, attempt)
	}
}
