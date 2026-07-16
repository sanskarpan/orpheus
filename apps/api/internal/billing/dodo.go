package billing

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DodoProvider integrates Dodo Payments (https://dodopayments.com), a
// payment gateway with first-class support for Indian + international
// collection. It creates hosted checkouts over the REST API and verifies
// inbound webhooks with the Standard Webhooks scheme Dodo uses.
//
// The wire shapes below follow Dodo's documented payments API. Because the
// integration sits behind the Provider interface and is exercised in tests
// via StubProvider, a minor field-name drift in Dodo's API is a localized
// change here rather than a cross-cutting one.
type DodoProvider struct {
	apiKey        string
	webhookSecret string
	baseURL       string
	http          *http.Client
	now           func() time.Time
}

// NewDodoProvider builds a Dodo provider. baseURL is the API root (e.g.
// https://live.dodopayments.com or https://test.dodopayments.com); it
// defaults to the test host when empty.
func NewDodoProvider(apiKey, webhookSecret, baseURL string) *DodoProvider {
	if baseURL == "" {
		baseURL = "https://test.dodopayments.com"
	}
	return &DodoProvider{
		apiKey:        apiKey,
		webhookSecret: webhookSecret,
		baseURL:       strings.TrimRight(baseURL, "/"),
		http:          &http.Client{Timeout: 15 * time.Second},
		now:           time.Now,
	}
}

// Name implements Provider.
func (d *DodoProvider) Name() string { return "dodo" }

// dodoCheckoutRequest is the create-payment body. Amounts are in the
// currency's minor unit (cents), matching Dodo's API.
type dodoCheckoutRequest struct {
	AmountCents int64             `json:"amount"`
	Currency    string            `json:"currency"`
	Reference   string            `json:"reference_id"`
	Metadata    map[string]string `json:"metadata,omitempty"`
}

type dodoCheckoutResponse struct {
	PaymentID   string `json:"payment_id"`
	PaymentLink string `json:"payment_link"`
}

// CreateCheckout implements Provider by opening a Dodo hosted checkout.
func (d *DodoProvider) CreateCheckout(ctx context.Context, inv Invoice) (CheckoutResult, error) {
	if d.apiKey == "" {
		return CheckoutResult{}, fmt.Errorf("dodo: missing api key")
	}
	currency := inv.Currency
	if currency == "" {
		currency = "USD"
	}
	reqBody := dodoCheckoutRequest{
		// Round to the minor unit; invoice totals carry sub-cent precision.
		AmountCents: int64(inv.TotalUSD*100 + 0.5),
		Currency:    currency,
		Reference:   inv.ID,
		Metadata:    map[string]string{"org_id": inv.OrgID, "invoice_id": inv.ID},
	}
	buf, err := json.Marshal(reqBody)
	if err != nil {
		return CheckoutResult{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.baseURL+"/payments", bytes.NewReader(buf))
	if err != nil {
		return CheckoutResult{}, err
	}
	req.Header.Set("Authorization", "Bearer "+d.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := d.http.Do(req)
	if err != nil {
		return CheckoutResult{}, fmt.Errorf("dodo: create checkout: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return CheckoutResult{}, fmt.Errorf("dodo: create checkout: status %d: %s", resp.StatusCode, string(body))
	}

	var out dodoCheckoutResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return CheckoutResult{}, fmt.Errorf("dodo: decode checkout: %w", err)
	}
	if out.PaymentLink == "" || out.PaymentID == "" {
		return CheckoutResult{}, fmt.Errorf("dodo: incomplete checkout response")
	}
	return CheckoutResult{CheckoutURL: out.PaymentLink, ProviderRef: out.PaymentID}, nil
}

// VerifyAndParse implements Provider: authenticate the Standard Webhooks
// signature, then normalize the Dodo event.
func (d *DodoProvider) VerifyAndParse(headers http.Header, body []byte) (WebhookEvent, error) {
	if err := verifyStandardWebhook(d.webhookSecret, headers, body, d.now()); err != nil {
		return WebhookEvent{}, err
	}
	return parseDodoEvent(body)
}

// dodoWebhookBody is the subset of a Dodo webhook we act on.
type dodoWebhookBody struct {
	Type string `json:"type"`
	Data struct {
		PaymentID string `json:"payment_id"`
		Status    string `json:"status"`
	} `json:"data"`
}

// parseDodoEvent normalizes a Dodo webhook body into a WebhookEvent. Payment
// success/failure is inferred from the event type or the payment status so a
// small naming difference between the two still classifies correctly.
func parseDodoEvent(body []byte) (WebhookEvent, error) {
	var b dodoWebhookBody
	if err := json.Unmarshal(body, &b); err != nil {
		return WebhookEvent{}, fmt.Errorf("dodo: decode webhook: %w", err)
	}
	t := strings.ToLower(b.Type)
	status := strings.ToLower(b.Data.Status)
	paid := strings.Contains(t, "succeeded") || strings.Contains(t, "completed") ||
		status == "succeeded" || status == "completed" || status == "paid"
	failed := strings.Contains(t, "failed") || strings.Contains(t, "cancelled") ||
		status == "failed" || status == "cancelled"
	return WebhookEvent{
		Type:        b.Type,
		ProviderRef: b.Data.PaymentID,
		Paid:        paid,
		Failed:      failed,
	}, nil
}
