package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/orpheus/api/internal/audit"
	"github.com/orpheus/api/internal/auth"
)

// TestWebhookDebug_TestFireDetailReplayEnable drives the PRD 03 surface against
// a live database: test-fire (+ rate limit), enriched delivery detail with an
// attempt timeline, bulk replay by filter, and re-enable. RLS is load-bearing.
func TestWebhookDebug_TestFireDetailReplayEnable(t *testing.T) {
	sut := testArtifactDB(t)
	svc := testServiceDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	orgID := uuid.NewString()
	if _, err := svc.Exec(ctx, `INSERT INTO organizations (id,name,slug) VALUES ($1,$2,$2)`, orgID, "wd-"+orgID); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	epID := uuid.NewString()
	if _, err := svc.Exec(ctx, `INSERT INTO webhook_endpoints (id,org_id,url,secret,subscribed_events,active) VALUES ($1,$2,'https://example.com/hook','sekret','{*}',false)`, epID, orgID); err != nil {
		t.Fatalf("seed endpoint: %v", err)
	}
	t.Cleanup(func() {
		cctx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		_, _ = svc.Exec(cctx, `DELETE FROM webhook_delivery_attempts WHERE org_id=$1`, orgID)
		_, _ = svc.Exec(cctx, `DELETE FROM webhook_deliveries WHERE org_id=$1`, orgID)
		_, _ = svc.Exec(cctx, `DELETE FROM webhook_endpoints WHERE org_id=$1`, orgID)
		_, _ = svc.Exec(cctx, `DELETE FROM organizations WHERE id=$1`, orgID)
	})

	h := &WebhookHandler{DB: sut, Audit: audit.New(sut, nil)}
	route := func(method, pattern, url string, body []byte, fn http.HandlerFunc) *httptest.ResponseRecorder {
		r := chi.NewRouter()
		r.MethodFunc(method, pattern, func(w http.ResponseWriter, req *http.Request) {
			fn(w, withPrincipal(req, &auth.Principal{OrgID: orgID}))
		})
		var rdr *bytes.Reader
		if body != nil {
			rdr = bytes.NewReader(body)
		} else {
			rdr = bytes.NewReader(nil)
		}
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(method, url, rdr))
		return rec
	}

	// 1) Test-fire → 202, creates an is_test delivery (endpoint is disabled,
	//    which is allowed for debugging).
	rec := route(http.MethodPost, "/v1/webhooks/{id}/test", "/v1/webhooks/"+epID+"/test",
		[]byte(`{"event_type":"job.completed"}`), h.TestFire)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("test-fire = %d: %s", rec.Code, rec.Body.String())
	}
	var fired struct {
		DeliveryID string `json:"delivery_id"`
	}
	_ = json.NewDecoder(rec.Body).Decode(&fired)
	var isTest bool
	if err := svc.QueryRow(ctx, `SELECT is_test FROM webhook_deliveries WHERE id=$1`, fired.DeliveryID).Scan(&isTest); err != nil || !isTest {
		t.Fatalf("test delivery not flagged is_test (err=%v)", err)
	}

	// 2) Rate limit: fire up to the cap, then expect 429.
	for i := 0; i < testFireRateLimit+2; i++ {
		rr := route(http.MethodPost, "/v1/webhooks/{id}/test", "/v1/webhooks/"+epID+"/test",
			[]byte(`{"event_type":"job.completed"}`), h.TestFire)
		if i >= testFireRateLimit && rr.Code == http.StatusTooManyRequests {
			break
		}
		if i == testFireRateLimit+1 && rr.Code != http.StatusTooManyRequests {
			t.Fatalf("expected 429 after %d test-fires, got %d", testFireRateLimit, rr.Code)
		}
	}

	// 3) Seed a delivered delivery with a signature base string + 2 attempts.
	delID := uuid.NewString()
	if _, err := svc.Exec(ctx, `
		INSERT INTO webhook_deliveries (id,org_id,endpoint_id,event_type,event_id,payload,status,attempt_count,signature_base_string,response_body_snippet,response_status)
		VALUES ($1,$2,$3,'job.completed',gen_random_uuid(),'{"job_id":"x"}'::jsonb,'delivered',2,'1700000000.{"job_id":"x"}','OK',200)
	`, delID, orgID, epID); err != nil {
		t.Fatalf("seed delivery: %v", err)
	}
	for i := 1; i <= 2; i++ {
		if _, err := svc.Exec(ctx, `INSERT INTO webhook_delivery_attempts (id,delivery_id,org_id,attempt_no,status_code,duration_ms,error) VALUES (gen_random_uuid(),$1,$2,$3,$4,$5,$6)`,
			delID, orgID, i, 500*(2-i)+200, 42, ""); err != nil {
			t.Fatalf("seed attempt: %v", err)
		}
	}
	drec := route(http.MethodGet, "/v1/webhooks/{id}/deliveries/{delivery_id}", fmt.Sprintf("/v1/webhooks/%s/deliveries/%s", epID, delID), nil, h.GetDelivery)
	if drec.Code != http.StatusOK {
		t.Fatalf("get delivery = %d: %s", drec.Code, drec.Body.String())
	}
	var detail WebhookDeliveryDetail
	_ = json.NewDecoder(drec.Body).Decode(&detail)
	if detail.SignatureBaseString == "" || len(detail.Attempts) != 2 {
		t.Fatalf("detail missing sig base or attempts: %+v", detail)
	}
	if detail.RequestHeaders["Content-Type"] != "application/json" {
		t.Fatalf("detail missing request headers")
	}

	// 4) Seed 3 failed deliveries, bulk-replay by status → requeued 3.
	for i := 0; i < 3; i++ {
		if _, err := svc.Exec(ctx, `
			INSERT INTO webhook_deliveries (id,org_id,endpoint_id,event_type,event_id,payload,status,attempt_count,max_attempts)
			VALUES (gen_random_uuid(),$1,$2,'job.failed',gen_random_uuid(),'{}'::jsonb,'failed',3,5)
		`, orgID, epID); err != nil {
			t.Fatalf("seed failed delivery: %v", err)
		}
	}
	brec := route(http.MethodPost, "/v1/webhooks/{id}/deliveries/replay", "/v1/webhooks/"+epID+"/deliveries/replay",
		[]byte(`{"status":"failed"}`), h.BulkReplay)
	if brec.Code != http.StatusAccepted {
		t.Fatalf("bulk replay = %d: %s", brec.Code, brec.Body.String())
	}
	var replayed struct {
		Requeued int `json:"requeued"`
	}
	_ = json.NewDecoder(brec.Body).Decode(&replayed)
	if replayed.Requeued != 3 {
		t.Fatalf("requeued = %d, want 3", replayed.Requeued)
	}

	// 5) Enable the (disabled) endpoint.
	erec := route(http.MethodPost, "/v1/webhooks/{id}/enable", "/v1/webhooks/"+epID+"/enable", nil, h.Enable)
	if erec.Code != http.StatusOK {
		t.Fatalf("enable = %d: %s", erec.Code, erec.Body.String())
	}
	var active bool
	var cf int
	if err := svc.QueryRow(ctx, `SELECT active, consecutive_failures FROM webhook_endpoints WHERE id=$1`, epID).Scan(&active, &cf); err != nil {
		t.Fatalf("read endpoint: %v", err)
	}
	if !active || cf != 0 {
		t.Fatalf("after enable active=%v cf=%d, want true/0", active, cf)
	}
}
