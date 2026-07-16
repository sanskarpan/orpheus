// Package billing turns recorded per-job cost into invoices and collects
// payment through a pluggable provider.
//
// The rollup (see rollup.go) aggregates each org's completed-job cost for a
// period into an `invoices` row. A Provider then creates a hosted checkout
// for an open invoice and verifies the inbound webhook that confirms
// payment. The provider is an interface so the Dodo Payments integration
// (dodo.go) and the deterministic test double (stub.go) are swappable, and
// so a binary started without payment credentials degrades cleanly.
package billing

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Invoice is the minimal invoice view a Provider needs to open a checkout.
// It intentionally excludes tenant-internal columns.
type Invoice struct {
	ID          string
	OrgID       string
	TotalUSD    float64
	Currency    string
	PeriodStart time.Time
	PeriodEnd   time.Time
}

// CheckoutResult is what a Provider returns after creating a hosted checkout.
type CheckoutResult struct {
	// CheckoutURL is the hosted page the customer is redirected to.
	CheckoutURL string
	// ProviderRef is the provider's id for the payment/checkout. It is
	// stored on the invoice so the inbound webhook can find the row again.
	ProviderRef string
}

// WebhookEvent is the normalized result of verifying + parsing an inbound
// provider webhook. Only the fields the billing flow acts on are surfaced.
type WebhookEvent struct {
	// Type is the provider's raw event type (e.g. "payment.succeeded").
	Type string
	// ProviderRef ties the event back to an invoice via invoices.provider_ref.
	ProviderRef string
	// Paid is true when the event means the invoice is now settled.
	Paid bool
	// Failed is true when the event means the payment failed.
	Failed bool
}

// Provider is a payment backend. Implementations must be safe for concurrent
// use.
type Provider interface {
	// Name identifies the provider (stored on invoices.provider).
	Name() string
	// CreateCheckout opens a hosted checkout for an open invoice.
	CreateCheckout(ctx context.Context, inv Invoice) (CheckoutResult, error)
	// VerifyAndParse authenticates a raw inbound webhook (headers + body)
	// and returns the normalized event. It returns an error if the
	// signature is invalid or the timestamp is outside tolerance.
	VerifyAndParse(headers http.Header, body []byte) (WebhookEvent, error)
}

// ErrInvalidSignature is returned when a webhook fails authentication.
var ErrInvalidSignature = errors.New("billing: invalid webhook signature")

// --- Standard Webhooks verification -----------------------------------------
//
// Dodo Payments signs webhooks with the Standard Webhooks spec
// (https://www.standardwebhooks.com): the signed content is
// "{id}.{timestamp}.{body}", the signature is base64(HMAC-SHA256(content,
// secret)), and it is delivered in the `webhook-signature` header as a
// space-separated list of `v1,<sig>` entries so keys can be rotated. The
// secret is shared out of band, optionally prefixed with `whsec_` and
// base64-encoded. We keep this in the package (not a third-party dep) so the
// verification is auditable and the stub can reuse it.

const (
	webhookTolerance = 5 * time.Minute

	hdrWebhookID        = "webhook-id"
	hdrWebhookTimestamp = "webhook-timestamp"
	hdrWebhookSignature = "webhook-signature"
)

// decodeSecret returns the raw HMAC key from a Standard Webhooks secret. A
// `whsec_`-prefixed value is base64-decoded; anything else is used verbatim
// (so tests can pass a plain string).
func decodeSecret(secret string) []byte {
	s := strings.TrimPrefix(secret, "whsec_")
	if raw, err := base64.StdEncoding.DecodeString(s); err == nil {
		return raw
	}
	return []byte(secret)
}

// signStandardWebhook computes the base64 v1 signature for an id+timestamp+body.
func signStandardWebhook(secret, id string, ts int64, body []byte) string {
	mac := hmac.New(sha256.New, decodeSecret(secret))
	// hash.Hash.Write never returns an error.
	_, _ = fmt.Fprintf(mac, "%s.%d.", id, ts)
	_, _ = mac.Write(body)
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// verifyStandardWebhook authenticates a Standard Webhooks request. It checks
// the timestamp is within tolerance and that at least one supplied v1
// signature matches, using a constant-time compare.
func verifyStandardWebhook(secret string, headers http.Header, body []byte, now time.Time) error {
	id := headers.Get(hdrWebhookID)
	tsRaw := headers.Get(hdrWebhookTimestamp)
	sigHeader := headers.Get(hdrWebhookSignature)
	if id == "" || tsRaw == "" || sigHeader == "" {
		return ErrInvalidSignature
	}

	ts, err := strconv.ParseInt(tsRaw, 10, 64)
	if err != nil {
		return ErrInvalidSignature
	}
	drift := now.Sub(time.Unix(ts, 0))
	if drift < -webhookTolerance || drift > webhookTolerance {
		return fmt.Errorf("%w: timestamp outside tolerance", ErrInvalidSignature)
	}

	expected := signStandardWebhook(secret, id, ts, body)
	// The header is a space-separated list of "v<version>,<base64sig>". Any
	// v1 entry matching the expected signature authenticates the request.
	for _, part := range strings.Fields(sigHeader) {
		version, sig, ok := strings.Cut(part, ",")
		if !ok || version != "v1" {
			continue
		}
		if hmac.Equal([]byte(sig), []byte(expected)) {
			return nil
		}
	}
	return ErrInvalidSignature
}
