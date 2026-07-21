// Package server is the HTTP transport layer for the Orpheus API.
//
// It wires handlers, middleware, and the underlying *http.Server together
// and exposes a single [Server.Run] entry point that blocks until SIGINT
// or SIGTERM and then performs a graceful shutdown.
package server

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/orpheus/api/internal/audit"
	"github.com/orpheus/api/internal/auth"
	"github.com/orpheus/api/internal/billing"
	"github.com/orpheus/api/internal/config"
	"github.com/orpheus/api/internal/db"
	"github.com/orpheus/api/internal/delivery"
	"github.com/orpheus/api/internal/handlers"
	"github.com/orpheus/api/internal/idempotency"
	"github.com/orpheus/api/internal/metrics"
	"github.com/orpheus/api/internal/ratelimit"
	"github.com/orpheus/api/internal/storage/s3"
)

// Options bundles the optional dependencies the /v1 surface needs.
// Each field may be nil; the v1 routes are mounted only when the
// authenticator is non-nil. The other fields are wired in when
// present so a test binary can omit, say, the audit recorder without
// breaking the rest of the router.
type Options struct {
	DB          *db.DB
	S3          *s3.Client
	Authn       *auth.Authenticator
	Idempotency *idempotency.Middleware
	RateLimit   *ratelimit.Middleware
	Audit       *audit.Recorder
	Metrics     *metrics.Metrics
	Billing     billing.Provider
	Deliverer   *delivery.Deliverer
	Scanner     handlers.AVScanner
}

// Server is the HTTP server for the Orpheus API.
//
// The zero value is NOT usable; construct via [New] for the public
// surface only, or via [NewWithOptions] for the full /v1 surface.
type Server struct {
	cfg    *config.Config
	logger *slog.Logger
	mux    *chi.Mux
	http   *http.Server

	opts Options
}

// New constructs a Server with the public surface only. It does not
// mount the /v1 routes — call [NewWithOptions] for the full server.
// It does not start listening; call [Server.Run] for that.
func New(cfg *config.Config, logger *slog.Logger) *Server {
	s := &Server{
		cfg:    cfg,
		logger: logger,
		mux:    chi.NewRouter(),
	}
	s.installBaseMiddleware()
	s.routes()
	s.installHTTP(cfg)
	return s
}

// NewWithOptions constructs a Server with the full /v1 surface in
// addition to the public routes. The /v1 routes are mounted only
// when opts.Authn is non-nil. Call [Server.Run] to start serving.
func NewWithOptions(cfg *config.Config, logger *slog.Logger, opts Options) *Server {
	s := &Server{
		cfg:    cfg,
		logger: logger,
		mux:    chi.NewRouter(),
		opts:   opts,
	}
	s.installBaseMiddleware()
	s.routes()
	if opts.Authn != nil {
		s.v1Routes()
	}
	s.installHTTP(cfg)
	return s
}

func (s *Server) installHTTP(cfg *config.Config) {
	s.http = &http.Server{
		Addr:              cfg.Addr(),
		Handler:           otelhttp.NewHandler(s.mux, "orpheus-api"),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
}

// installBaseMiddleware attaches the chi-provided middlewares that
// every request sees, regardless of the route. Kept separate from
// [Server.routes] so the v1 router can stack on top of the same base.
func (s *Server) installBaseMiddleware() {
	s.mux.Use(middleware.RequestID)
	s.mux.Use(middleware.Recoverer)
	s.mux.Use(middleware.Timeout(30 * time.Second))
	if s.opts.Metrics != nil {
		s.mux.Use(MetricsMiddleware(s.opts.Metrics))
	}
}

// routes installs the public surface: liveness, readiness, metrics,
// and the OpenAPI documentation. These endpoints do not require
// authentication and are mounted before the v1 router so a hostile
// client cannot discover them via a /v1 scan.
//
// Note: middleware.RealIP is intentionally omitted. It was deprecated in
// chi v5.3.1 (GHSA-3fxj-6jh8-hvhx) because it trusts X-Forwarded-For
// without verifying the request came from a trusted proxy. Phase 1+ will
// add a vetted client-IP helper once a load balancer is in front of the
// service.
func (s *Server) routes() {
	s.mux.Get("/health", handlers.Liveness)
	s.mux.Get("/ready", handlers.Readiness)
	// Serve /metrics from the per-instance registry when available (the
	// collectors are no longer on the global default registry, so
	// promhttp.Handler() would be empty and New() is now repeatable).
	if s.opts.Metrics != nil && s.opts.Metrics.Registry != nil {
		s.mux.Get("/metrics", promhttp.HandlerFor(s.opts.Metrics.Registry, promhttp.HandlerOpts{}).ServeHTTP)
	} else {
		s.mux.Get("/metrics", promhttp.Handler().ServeHTTP)
	}

	s.mux.Route("/api", func(r chi.Router) {
		r.Get("/openapi.json", handlers.OpenAPISpec)
		r.Get("/docs", handlers.SwaggerUI)
		r.Get("/redoc", handlers.ReDocUI)
	})

	// Inbound payment-provider webhook. It is public at the transport
	// layer (no API key) but authenticated by the provider's signed
	// payload inside the handler. Mounted only when billing is configured.
	if s.opts.DB != nil && s.opts.Billing != nil {
		bh := &handlers.BillingHandler{DB: s.opts.DB, Audit: s.opts.Audit, Provider: s.opts.Billing}
		s.mux.Post("/billing/webhooks/dodo", bh.DodoWebhook)
	}
}

// v1Routes mounts the authenticated /v1 surface. The order of
// middleware matters and matches the design doc:
//
//   - Authn.Middleware resolves the principal and attaches it to the
//     context. Everything downstream reads it via auth.PrincipalFromContext.
//   - RateLimit.Handler enforces per-org / per-key quotas. Must run
//     after Authn so the bucket key includes the principal.
//   - Idempotency.Handler caches responses to POSTs that carry an
//     Idempotency-Key header. Must run after Authn so the org-scoped
//     lookup has a principal.
//   - Audit.Middleware records every state-changing request to the
//     audit log. Runs last so the recorded status reflects the
//     response the client actually saw.
func (s *Server) v1Routes() {
	s.mux.Route("/v1", func(r chi.Router) {
		r.Use(s.opts.Authn.Middleware)
		if s.opts.RateLimit != nil {
			r.Use(s.opts.RateLimit.Handler)
		}
		if s.opts.Idempotency != nil {
			r.Use(s.opts.Idempotency.Handler)
		}
		if s.opts.Audit != nil {
			r.Use(s.opts.Audit.Middleware)
		}

		// Per-route scope enforcement. JWT principals have full org
		// authority; API-key principals must hold the listed scope (or
		// "*"). RequireScope is a no-op for unscoped tokens by design.
		rs := auth.RequireScope

		uh := &handlers.UploadHandler{DB: s.opts.DB, S3: s.opts.S3, Audit: s.opts.Audit, Scanner: s.opts.Scanner}
		r.With(rs("uploads:write")).Post("/uploads", uh.Create)
		r.With(rs("uploads:write")).Post("/uploads/url", uh.CreateURLIngest)
		r.With(rs("uploads:write")).Post("/uploads/{id}/complete", uh.Complete)
		r.With(rs("uploads:read")).Get("/uploads/{id}/parts", uh.GetParts)
		r.With(rs("uploads:write")).Post("/uploads/{id}/parts:refresh", uh.RefreshParts)
		r.With(rs("uploads:read")).Get("/uploads", uh.List)
		r.With(rs("uploads:read")).Get("/uploads/{id}", uh.Get)

		ah := &handlers.ArtifactHandler{DB: s.opts.DB, S3: s.opts.S3}
		r.With(rs("artifacts:read")).Get("/artifacts", ah.List)
		r.With(rs("artifacts:read")).Get("/artifacts/{id}", ah.Get)
		r.With(rs("artifacts:read")).Get("/artifacts/{id}/signed-url", ah.GetSignedURL)

		jh := &handlers.JobHandler{DB: s.opts.DB, Audit: s.opts.Audit, Metrics: s.opts.Metrics}
		r.With(rs("jobs:write")).Post("/jobs", jh.Create)
		r.With(rs("jobs:write")).Post("/jobs/bulk", jh.BulkCreate)
		r.With(rs("jobs:read")).Get("/jobs", jh.List)
		r.With(rs("jobs:read")).Get("/jobs/{id}", jh.Get)
		r.With(rs("jobs:write")).Delete("/jobs/{id}", jh.Cancel)
		r.With(rs("jobs:write")).Post("/jobs/{id}/requeue", jh.Requeue)

		ph := &handlers.ProcessorHandler{DB: s.opts.DB}
		// The processor catalog is public within the org; no scope gate.
		r.Get("/processors", ph.List)
		r.Get("/processors/{name}", ph.Get)

		wh := &handlers.WebhookHandler{DB: s.opts.DB, Audit: s.opts.Audit}
		r.With(rs("webhooks:write")).Post("/webhooks", wh.Create)
		r.With(rs("webhooks:read")).Get("/webhooks", wh.List)
		r.With(rs("webhooks:read")).Get("/webhooks/{id}", wh.Get)
		r.With(rs("webhooks:write")).Patch("/webhooks/{id}", wh.Update)
		r.With(rs("webhooks:write")).Delete("/webhooks/{id}", wh.Delete)
		r.With(rs("webhooks:read")).Get("/webhooks/{id}/deliveries", wh.ListDeliveries)
		r.With(rs("webhooks:read")).Get("/webhooks/{id}/deliveries/{delivery_id}", wh.GetDelivery)
		r.With(rs("webhooks:write")).Post("/webhooks/{id}/deliveries/{delivery_id}/replay", wh.Replay)
		r.With(rs("webhooks:write")).Post("/webhooks/{id}/deliveries/replay", wh.BulkReplay)
		r.With(rs("webhooks:write")).Post("/webhooks/{id}/test", wh.TestFire)
		r.With(rs("webhooks:write")).Post("/webhooks/{id}/enable", wh.Enable)

		// API-key management is an admin operation. There is no
		// api_keys scope in the enum, so require the "*" wildcard: a
		// scoped API key cannot manage keys, only a full-access token can.
		kh := &handlers.APIKeyHandler{DB: s.opts.DB, Audit: s.opts.Audit}
		r.With(rs("*")).Post("/api-keys", kh.Create)
		r.With(rs("*")).Get("/api-keys", kh.List)
		r.With(rs("*")).Delete("/api-keys/{id}", kh.Revoke)

		wh2 := &handlers.WorkflowHandler{DB: s.opts.DB, Audit: s.opts.Audit}
		r.With(rs("jobs:write")).Post("/workflows/transcribe-long", wh2.CreateTranscribeLong)
		r.With(rs("jobs:read")).Get("/workflows/{id}", wh2.Get)

		strh := &handlers.StreamingHandler{DB: s.opts.DB, Audit: s.opts.Audit}
		r.With(rs("streaming:write")).Post("/streaming/sessions", strh.Create)
		r.With(rs("streaming:read")).Get("/streaming/sessions", strh.List)
		r.With(rs("streaming:read")).Get("/streaming/sessions/{id}", strh.Get)
		r.With(rs("streaming:write")).Post("/streaming/sessions/{id}/finalize", strh.Finalize)

		sh := &handlers.SystemHandler{DB: s.opts.DB}
		r.With(rs("usage:read")).Get("/usage", sh.GetUsage)
		r.With(rs("audit:read")).Get("/audit-log", sh.ListAuditLog)

		uth := &handlers.UsageTimeseriesHandler{DB: s.opts.DB}
		r.With(rs("usage:read")).Get("/usage/timeseries", uth.GetTimeseries)

		budh := &handlers.BudgetHandler{DB: s.opts.DB, Audit: s.opts.Audit}
		r.With(rs("usage:read")).Get("/budgets", budh.List)
		r.With(rs("billing:write")).Post("/budgets", budh.Create)
		r.With(rs("billing:write")).Patch("/budgets/{id}", budh.Update)
		r.With(rs("billing:write")).Delete("/budgets/{id}", budh.Delete)

		bh := &handlers.BillingHandler{DB: s.opts.DB, Audit: s.opts.Audit, Provider: s.opts.Billing}
		r.With(rs("billing:read")).Get("/billing/invoices", bh.ListInvoices)
		r.With(rs("billing:write")).Post("/billing/invoices/{id}/checkout", bh.CreateCheckout)

		erh := &handlers.ErasureHandler{DB: s.opts.DB, Audit: s.opts.Audit}
		r.With(rs("data:erase")).Post("/erasure-requests", erh.Create)
		r.With(rs("data:erase")).Get("/erasure-requests", erh.List)
		r.With(rs("data:erase")).Get("/erasure-requests/{id}", erh.Get)

		ch := &handlers.CacheHandler{DB: s.opts.DB}
		r.With(rs("usage:read")).Get("/cache/stats", ch.Stats)
		// Cache invalidation is an admin operation (purge a model version).
		r.With(rs("*")).Delete("/cache", ch.Invalidate)

		bth := &handlers.BatchHandler{DB: s.opts.DB, Audit: s.opts.Audit, S3: s.opts.S3}
		r.With(rs("jobs:write")).Post("/batches", bth.Create)
		r.With(rs("jobs:read")).Get("/batches/{id}", bth.Get)
		r.With(rs("jobs:read")).Get("/batches/{id}/jobs", bth.ListJobs)
		r.With(rs("jobs:read")).Get("/batches/{id}/manifest", bth.Manifest)

		dsh := &handlers.DestinationHandler{DB: s.opts.DB, Audit: s.opts.Audit, Deliverer: s.opts.Deliverer}
		r.With(rs("jobs:write")).Post("/destinations", dsh.Create)
		r.With(rs("jobs:read")).Get("/destinations", dsh.List)
		r.With(rs("jobs:write")).Post("/destinations/{id}/verify", dsh.Verify)
		r.With(rs("jobs:write")).Delete("/destinations/{id}", dsh.Delete)

		bnh := &handlers.BundleHandler{DB: s.opts.DB, S3: s.opts.S3, Audit: s.opts.Audit}
		r.With(rs("artifacts:read")).Post("/bundles", bnh.Create)
		r.With(rs("artifacts:read")).Get("/bundles", bnh.List)
		r.With(rs("artifacts:read")).Get("/bundles/{id}", bnh.Get)
		r.With(rs("artifacts:read")).Get("/bundles/{id}/download", bnh.Download)
		r.With(rs("artifacts:read")).Delete("/bundles/{id}", bnh.Delete)
	})
}

// Run starts the HTTP server and blocks until ctx is cancelled or the
// server fails to start. On cancellation it performs a graceful shutdown
// bounded by cfg.ShutdownGraceSeconds.
//
// Run does not return on a clean shutdown signal; it only returns an
// error on unexpected failure (port already in use, shutdown timeout, etc.).
func (s *Server) Run(ctx context.Context) error {
	listenErr := make(chan error, 1)
	go func() {
		s.logger.Info("orpheus_api.http.listening",
			"addr", s.cfg.Addr(),
			"env", s.cfg.Env,
		)
		if err := s.http.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			listenErr <- err
		}
		close(listenErr)
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(
			context.Background(),
			time.Duration(s.cfg.ShutdownGraceSeconds)*time.Second,
		)
		defer cancel()

		s.logger.Info("orpheus_api.http.shutting_down",
			"grace_seconds", s.cfg.ShutdownGraceSeconds,
		)
		if err := s.http.Shutdown(shutdownCtx); err != nil {
			return err
		}
		// Drain the listen goroutine so it does not leak.
		<-listenErr
		return nil
	case err := <-listenErr:
		if err != nil {
			return err
		}
		return nil
	}
}
