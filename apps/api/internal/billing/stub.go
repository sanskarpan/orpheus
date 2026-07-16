package billing

import (
	"context"
	"net/http"
	"time"
)

// StubProvider is a deterministic, network-free Provider for tests and for
// running the billing surface without real payment credentials. Checkouts
// resolve to a local-looking URL; webhooks are still authenticated with the
// same Standard Webhooks verification the real provider uses, so webhook
// handling is exercised end to end with a known secret.
type StubProvider struct {
	Secret string
	// now is injectable so tests can pin the webhook timestamp window.
	now func() time.Time
}

// NewStubProvider builds a stub provider verifying webhooks against secret.
func NewStubProvider(secret string) *StubProvider {
	return &StubProvider{Secret: secret, now: time.Now}
}

// Name implements Provider.
func (s *StubProvider) Name() string { return "stub" }

// CreateCheckout implements Provider with a deterministic result derived
// from the invoice id — no network call.
func (s *StubProvider) CreateCheckout(_ context.Context, inv Invoice) (CheckoutResult, error) {
	ref := "stub_" + inv.ID
	return CheckoutResult{
		CheckoutURL: "https://checkout.test/invoice/" + inv.ID,
		ProviderRef: ref,
	}, nil
}

// VerifyAndParse implements Provider, reusing the shared Standard Webhooks
// verification and Dodo event shape.
func (s *StubProvider) VerifyAndParse(headers http.Header, body []byte) (WebhookEvent, error) {
	now := time.Now
	if s.now != nil {
		now = s.now
	}
	if err := verifyStandardWebhook(s.Secret, headers, body, now()); err != nil {
		return WebhookEvent{}, err
	}
	return parseDodoEvent(body)
}
