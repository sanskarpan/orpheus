package webhooks

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestRecordAttemptAndAutoDisable exercises the PRD 03 write path directly:
// recordAttempt appends the per-attempt row and stamps the signature base
// string; recordEndpointOutcome increments the failure counter and
// auto-disables at the threshold, and resets on success.
func TestRecordAttemptAndAutoDisable(t *testing.T) {
	dsn := os.Getenv("ORPHEUS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ORPHEUS_TEST_DATABASE_URL not set; skipping webhook debug write-path test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	svc := webhookServicePool(t, dsn)
	orgID, endpointID := seedEndpoint(t, ctx, svc, "https://example.com/hook", "sekret")
	s := newDelivery(svc, nil)

	// A delivery row for the attempt to reference + update.
	delID := uuid.NewString()
	if _, err := svc.Exec(ctx, `
		INSERT INTO webhook_deliveries (id, org_id, endpoint_id, event_type, event_id, payload, status, attempt_count, max_attempts)
		VALUES ($1, $2, $3, 'job.queued', gen_random_uuid(), '{}'::jsonb, 'delivering', 1, 24)
	`, delID, orgID, endpointID); err != nil {
		t.Fatalf("seed delivery: %v", err)
	}

	// recordAttempt writes the timeline row + the signature base string.
	d := claimed{ID: delID, OrgID: orgID, EndpointID: endpointID, AttemptCount: 1}
	s.recordAttempt(ctx, d, 1, 503, 37, "503 Service Unavailable", "1700000000.{}", []byte("upstream boom"))
	var attemptCount int
	var sigBase, snippet string
	if err := svc.QueryRow(ctx, `SELECT COUNT(*) FROM webhook_delivery_attempts WHERE delivery_id=$1`, delID).Scan(&attemptCount); err != nil {
		t.Fatalf("count attempts: %v", err)
	}
	if attemptCount != 1 {
		t.Fatalf("attempts = %d, want 1", attemptCount)
	}
	if err := svc.QueryRow(ctx, `SELECT signature_base_string, response_body_snippet FROM webhook_deliveries WHERE id=$1`, delID).Scan(&sigBase, &snippet); err != nil {
		t.Fatalf("read delivery: %v", err)
	}
	if sigBase != "1700000000.{}" || snippet != "upstream boom" {
		t.Fatalf("sig/snippet = %q / %q", sigBase, snippet)
	}

	// Auto-disable: push to one below the threshold, then one failure flips it.
	if _, err := svc.Exec(ctx, `UPDATE webhook_endpoints SET consecutive_failures=$2 WHERE id=$1`, endpointID, autoDisableThreshold-1); err != nil {
		t.Fatalf("preset counter: %v", err)
	}
	s.recordEndpointOutcome(ctx, endpointID, false)
	var active bool
	var cf int
	if err := svc.QueryRow(ctx, `SELECT active, consecutive_failures FROM webhook_endpoints WHERE id=$1`, endpointID).Scan(&active, &cf); err != nil {
		t.Fatalf("read endpoint: %v", err)
	}
	if active || cf != autoDisableThreshold {
		t.Fatalf("after threshold failure active=%v cf=%d, want false/%d", active, cf, autoDisableThreshold)
	}

	// Success resets the counter.
	s.recordEndpointOutcome(ctx, endpointID, true)
	if err := svc.QueryRow(ctx, `SELECT consecutive_failures FROM webhook_endpoints WHERE id=$1`, endpointID).Scan(&cf); err != nil {
		t.Fatalf("read endpoint: %v", err)
	}
	if cf != 0 {
		t.Fatalf("after success cf=%d, want 0", cf)
	}
}
