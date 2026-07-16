package handlers

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/orpheus/api/internal/auth"
	"github.com/orpheus/api/internal/billing"
)

// signStdWebhook mirrors the Standard Webhooks signing the billing package
// verifies, so the test can produce a valid inbound webhook. A `whsec_`
// secret is base64-decoded; anything else is used verbatim.
func signStdWebhook(secret, id string, ts int64, body []byte) string {
	key := []byte(strings.TrimPrefix(secret, "whsec_"))
	if raw, err := base64.StdEncoding.DecodeString(string(key)); err == nil {
		key = raw
	}
	mac := hmac.New(sha256.New, key)
	_, _ = fmt.Fprintf(mac, "%s.%d.", id, ts)
	_, _ = mac.Write(body)
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// TestBilling_ListCheckoutWebhook drives the billing surface end to end
// against a live database: a tenant lists their invoice, opens a checkout
// (stub provider), then the provider's signed webhook settles it. It uses
// the same tenant-scoped SUT pool as the other handler tests, so RLS is
// load-bearing throughout.
func TestBilling_ListCheckoutWebhook(t *testing.T) {
	sut := testArtifactDB(t) // tenant-scoped (RLS enforced)
	svc := testServiceDB(t)  // service-role, seed/cleanup only
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	orgID := uuid.NewString()
	if _, err := svc.Exec(ctx, `INSERT INTO organizations (id,name,slug) VALUES ($1,$2,$2)`, orgID, "bh-"+orgID); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	otherOrg := uuid.NewString()
	_, _ = svc.Exec(ctx, `INSERT INTO organizations (id,name,slug) VALUES ($1,$2,$2)`, otherOrg, "bh-"+otherOrg)
	t.Cleanup(func() {
		cctx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		_, _ = svc.Exec(cctx, `DELETE FROM invoices WHERE org_id IN ($1,$2)`, orgID, otherOrg)
		_, _ = svc.Exec(cctx, `DELETE FROM organizations WHERE id IN ($1,$2)`, orgID, otherOrg)
	})

	// Seed an open invoice for our org and one for another org (which must
	// never appear in our tenant-scoped list).
	invID := uuid.NewString()
	start := time.Date(2098, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 1, 0)
	if _, err := svc.Exec(ctx, `
		INSERT INTO invoices (id, org_id, period_start, period_end, jobs_count, compute_seconds, total_usd, status)
		VALUES ($1, $2, $3, $4, 3, 90, 4.50, 'open')
	`, invID, orgID, start, end); err != nil {
		t.Fatalf("seed invoice: %v", err)
	}
	_, _ = svc.Exec(ctx, `
		INSERT INTO invoices (id, org_id, period_start, period_end, total_usd, status)
		VALUES (gen_random_uuid(), $1, $2, $3, 99, 'open')
	`, otherOrg, start, end)

	// Build the secret at runtime so no secret-shaped literal sits in source.
	secret := "whsec_" + base64.StdEncoding.EncodeToString([]byte("testsecret"))
	provider := billing.NewStubProvider(secret)
	h := &BillingHandler{DB: sut, Provider: provider}

	// 1) List — sees only our org's invoice.
	{
		req := withPrincipal(httptest.NewRequest(http.MethodGet, "/v1/billing/invoices", nil), &auth.Principal{OrgID: orgID})
		rec := httptest.NewRecorder()
		h.ListInvoices(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("ListInvoices = %d: %s", rec.Code, rec.Body.String())
		}
		var list InvoiceList
		_ = json.NewDecoder(rec.Body).Decode(&list)
		if len(list.Data) != 1 || list.Data[0].ID != invID {
			t.Fatalf("list = %+v, want exactly our invoice %s (RLS leak?)", list.Data, invID)
		}
	}

	// 2) Checkout — opens a hosted checkout and records the provider ref.
	var providerRef string
	{
		r := chi.NewRouter()
		r.Post("/v1/billing/invoices/{id}/checkout", func(w http.ResponseWriter, req *http.Request) {
			h.CreateCheckout(w, withPrincipal(req, &auth.Principal{OrgID: orgID}))
		})
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/billing/invoices/"+invID+"/checkout", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("CreateCheckout = %d: %s", rec.Code, rec.Body.String())
		}
		var cr CheckoutResponse
		_ = json.NewDecoder(rec.Body).Decode(&cr)
		if cr.CheckoutURL == "" {
			t.Fatal("empty checkout url")
		}
		if err := svc.QueryRow(ctx, `SELECT provider_ref FROM invoices WHERE id=$1`, invID).Scan(&providerRef); err != nil {
			t.Fatalf("read provider_ref: %v", err)
		}
		if providerRef == "" {
			t.Fatal("provider_ref not persisted")
		}
	}

	// 3) Webhook — a signed payment.succeeded settles the invoice.
	{
		body := []byte(`{"type":"payment.succeeded","data":{"payment_id":"` + providerRef + `","status":"succeeded"}}`)
		ts := time.Now().Unix()
		// Sign with the same Standard Webhooks scheme the provider verifies.
		req := httptest.NewRequest(http.MethodPost, "/billing/webhooks/dodo", strings.NewReader(string(body)))
		req.Header.Set("webhook-id", "msg_it")
		req.Header.Set("webhook-timestamp", strconv.FormatInt(ts, 10))
		req.Header.Set("webhook-signature", "v1,"+signStdWebhook(secret, "msg_it", ts, body))
		rec := httptest.NewRecorder()
		h.DodoWebhook(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("DodoWebhook = %d: %s", rec.Code, rec.Body.String())
		}
		var status string
		if err := svc.QueryRow(ctx, `SELECT status FROM invoices WHERE id=$1`, invID).Scan(&status); err != nil {
			t.Fatalf("read status: %v", err)
		}
		if status != "paid" {
			t.Fatalf("invoice status = %q, want paid", status)
		}
	}

	// 4) Webhook with a bad signature is rejected.
	{
		body := []byte(`{"type":"payment.succeeded","data":{"payment_id":"` + providerRef + `","status":"succeeded"}}`)
		req := httptest.NewRequest(http.MethodPost, "/billing/webhooks/dodo", strings.NewReader(string(body)))
		req.Header.Set("webhook-id", "msg_bad")
		req.Header.Set("webhook-timestamp", strconv.FormatInt(time.Now().Unix(), 10))
		req.Header.Set("webhook-signature", "v1,not-a-valid-signature")
		rec := httptest.NewRecorder()
		h.DodoWebhook(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("bad-signature webhook = %d, want 401", rec.Code)
		}
	}
}
