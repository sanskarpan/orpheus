// Package handlers — billing endpoints (#11).
//
// Two authenticated, tenant-scoped endpoints let a customer see their
// invoices and open a hosted checkout for one, and one public endpoint
// receives the payment provider's webhook. The webhook is unauthenticated at
// the transport layer (it carries no API key) but is authenticated by the
// provider's signed payload, then applied under the service role because it
// must find an invoice across tenants by the provider reference.
package handlers

import (
	"context"
	"io"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/orpheus/api/internal/audit"
	"github.com/orpheus/api/internal/auth"
	"github.com/orpheus/api/internal/billing"
	"github.com/orpheus/api/internal/db"
	"github.com/orpheus/api/internal/dbtx"
)

// BillingHandler bundles the dependencies the billing endpoints need. When
// Provider is nil the API was started without payment credentials; checkout
// then returns 503 and the webhook route is not mounted.
type BillingHandler struct {
	DB       *db.DB
	Audit    *audit.Recorder
	Provider billing.Provider
}

// InvoiceView is the wire shape for an invoice row.
type InvoiceView struct {
	ID             string     `json:"id"`
	PeriodStart    time.Time  `json:"period_start"`
	PeriodEnd      time.Time  `json:"period_end"`
	JobsCount      int        `json:"jobs_count"`
	ComputeSeconds float64    `json:"compute_seconds"`
	TotalUSD       float64    `json:"total_usd"`
	Status         string     `json:"status"`
	Provider       string     `json:"provider,omitempty"`
	CheckoutURL    string     `json:"checkout_url,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	PaidAt         *time.Time `json:"paid_at,omitempty"`
}

// InvoiceList is the response for GET /v1/billing/invoices.
type InvoiceList struct {
	Data []InvoiceView `json:"data"`
}

// ListInvoices handles GET /v1/billing/invoices — the caller's invoices,
// newest period first.
func (h *BillingHandler) ListInvoices(w http.ResponseWriter, r *http.Request) {
	p, err := auth.PrincipalFromContext(r.Context())
	if err != nil {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "Unauthorized")
		return
	}

	var invoices []InvoiceView
	err = h.DB.WithTenant(r.Context(), p.OrgID, func(ctx context.Context) error {
		rows, err := dbtx.Query(ctx, h.DB, `
			SELECT id, period_start, period_end, jobs_count, compute_seconds::float8,
			       total_usd::float8, status, COALESCE(provider, ''),
			       COALESCE(checkout_url, ''), created_at, paid_at
			FROM invoices
			WHERE org_id = $1
			ORDER BY period_start DESC
			LIMIT 200
		`, p.OrgID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var iv InvoiceView
			if err := rows.Scan(
				&iv.ID, &iv.PeriodStart, &iv.PeriodEnd, &iv.JobsCount, &iv.ComputeSeconds,
				&iv.TotalUSD, &iv.Status, &iv.Provider, &iv.CheckoutURL, &iv.CreatedAt, &iv.PaidAt,
			); err != nil {
				return err
			}
			invoices = append(invoices, iv)
		}
		return rows.Err()
	})
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "internal", "Failed to list invoices")
		return
	}
	writeJSON(w, http.StatusOK, InvoiceList{Data: invoices})
}

// CheckoutResponse is returned by the checkout endpoint.
type CheckoutResponse struct {
	CheckoutURL string `json:"checkout_url"`
}

// CreateCheckout handles POST /v1/billing/invoices/{id}/checkout. It opens a
// hosted checkout for an open invoice and stores the provider reference so
// the inbound webhook can settle it. Idempotent-ish: an invoice that already
// has a checkout URL returns it rather than opening a second one.
func (h *BillingHandler) CreateCheckout(w http.ResponseWriter, r *http.Request) {
	p, err := auth.PrincipalFromContext(r.Context())
	if err != nil {
		writeProblem(w, http.StatusUnauthorized, "unauthenticated", "Unauthorized")
		return
	}
	if h.Provider == nil {
		writeProblem(w, http.StatusServiceUnavailable, "billing_unconfigured", "Billing is not configured")
		return
	}
	id, ok := uuidParam(r, "id")
	if !ok {
		writeProblem(w, http.StatusNotFound, "not_found", "Invoice not found")
		return
	}

	// Load the invoice under tenant scope.
	var (
		status, existingURL    string
		totalUSD               float64
		periodStart, periodEnd time.Time
	)
	err = h.DB.WithTenant(r.Context(), p.OrgID, func(ctx context.Context) error {
		return dbtx.QueryRow(ctx, h.DB, `
			SELECT status, COALESCE(checkout_url, ''), total_usd::float8, period_start, period_end
			FROM invoices WHERE id = $1 AND org_id = $2
		`, id, p.OrgID).Scan(&status, &existingURL, &totalUSD, &periodStart, &periodEnd)
	})
	if err != nil {
		if err == pgx.ErrNoRows {
			writeProblem(w, http.StatusNotFound, "not_found", "Invoice not found")
			return
		}
		writeProblem(w, http.StatusInternalServerError, "internal", "Failed to load invoice")
		return
	}
	if status == "paid" || status == "void" {
		writeProblem(w, http.StatusConflict, "invalid_state", "Invoice is not payable")
		return
	}
	if existingURL != "" {
		writeJSON(w, http.StatusOK, CheckoutResponse{CheckoutURL: existingURL})
		return
	}

	res, err := h.Provider.CreateCheckout(r.Context(), billing.Invoice{
		ID: id, OrgID: p.OrgID, TotalUSD: totalUSD, Currency: "USD",
		PeriodStart: periodStart, PeriodEnd: periodEnd,
	})
	if err != nil {
		writeProblem(w, http.StatusBadGateway, "provider_error", "Payment provider error")
		return
	}

	err = h.DB.WithTenant(r.Context(), p.OrgID, func(ctx context.Context) error {
		_, e := dbtx.Exec(ctx, h.DB, `
			UPDATE invoices
			SET status = 'open', provider = $1, provider_ref = $2, checkout_url = $3, updated_at = now()
			WHERE id = $4 AND org_id = $5
		`, h.Provider.Name(), res.ProviderRef, res.CheckoutURL, id, p.OrgID)
		return e
	})
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "internal", "Failed to record checkout")
		return
	}

	if h.Audit != nil {
		_ = h.Audit.Record(r.Context(), audit.Entry{
			Action: "billing.payment", ResourceType: "invoice", ResourceID: id,
			Metadata: map[string]any{"provider": h.Provider.Name(), "action": "checkout"},
		})
	}
	writeJSON(w, http.StatusOK, CheckoutResponse{CheckoutURL: res.CheckoutURL})
}

// DodoWebhook handles POST /billing/webhooks/dodo. It authenticates the
// signed payload with the provider, then settles the matching invoice under
// the service role (the payload identifies the invoice across tenants by the
// provider reference). It always returns 200 on a verified event so the
// provider does not retry a delivery we have already accepted.
func (h *BillingHandler) DodoWebhook(w http.ResponseWriter, r *http.Request) {
	if h.Provider == nil {
		writeProblem(w, http.StatusServiceUnavailable, "billing_unconfigured", "Billing is not configured")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "validation", "Failed to read body")
		return
	}
	event, err := h.Provider.VerifyAndParse(r.Header, body)
	if err != nil {
		writeProblem(w, http.StatusUnauthorized, "invalid_signature", "Invalid webhook signature")
		return
	}
	if event.ProviderRef == "" || (!event.Paid && !event.Failed) {
		// Nothing actionable (e.g. an informational event) — accept it.
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored"})
		return
	}

	newStatus := "failed"
	if event.Paid {
		newStatus = "paid"
	}
	err = h.withServiceTx(r.Context(), func(tx pgx.Tx) error {
		_, e := tx.Exec(r.Context(), `
			UPDATE invoices
			SET status = $1,
			    paid_at = CASE WHEN $1 = 'paid' THEN now() ELSE paid_at END,
			    updated_at = now()
			WHERE provider_ref = $2 AND status NOT IN ('paid', 'void')
		`, newStatus, event.ProviderRef)
		return e
	})
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "internal", "Failed to apply webhook")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// withServiceTx runs fn with app.is_service set locally so the webhook can
// settle an invoice found by provider reference across tenants.
func (h *BillingHandler) withServiceTx(ctx context.Context, fn func(pgx.Tx) error) error {
	conn, err := h.DB.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()
	if _, err := tx.Exec(ctx, "SELECT set_config('app.is_service','true',true)"); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	committed = true
	return nil
}
