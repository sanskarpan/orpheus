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

	"github.com/orpheus/api/internal/audit"
	"github.com/orpheus/api/internal/auth"
	"github.com/orpheus/api/internal/config"
	"github.com/orpheus/api/internal/db"
	"github.com/orpheus/api/internal/handlers"
	"github.com/orpheus/api/internal/idempotency"
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
	s.http = &http.Server{
		Addr:              cfg.Addr(),
		Handler:           s.mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	return s
}

// NewWithOptions constructs a Server with the full /v1 surface in
// addition to the public routes. The /v1 routes are mounted only
// when opts.Authn is non-nil. Call [Server.Run] to start serving.
func NewWithOptions(cfg *config.Config, logger *slog.Logger, opts Options) *Server {
	s := New(cfg, logger)
	s.opts = opts
	if opts.Authn != nil {
		s.v1Routes()
	}
	return s
}

// installBaseMiddleware attaches the chi-provided middlewares that
// every request sees, regardless of the route. Kept separate from
// [Server.routes] so the v1 router can stack on top of the same base.
func (s *Server) installBaseMiddleware() {
	s.mux.Use(middleware.RequestID)
	s.mux.Use(middleware.Recoverer)
	s.mux.Use(middleware.Timeout(30 * time.Second))
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
	s.mux.Get("/metrics", handlers.Metrics)

	s.mux.Route("/api", func(r chi.Router) {
		r.Get("/openapi.json", handlers.OpenAPISpec)
		r.Get("/docs", handlers.SwaggerUI)
		r.Get("/redoc", handlers.ReDocUI)
	})
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

		uh := &handlers.UploadHandler{DB: s.opts.DB, S3: s.opts.S3, Audit: s.opts.Audit}
		r.Post("/uploads", uh.Create)
		r.Post("/uploads/{id}/complete", uh.Complete)
		r.Get("/uploads", uh.List)
		r.Get("/uploads/{id}", uh.Get)

		ah := &handlers.ArtifactHandler{DB: s.opts.DB, S3: s.opts.S3}
		r.Get("/artifacts", ah.List)
		r.Get("/artifacts/{id}", ah.Get)
		r.Get("/artifacts/{id}/signed-url", ah.GetSignedURL)

		jh := &handlers.JobHandler{DB: s.opts.DB, Audit: s.opts.Audit}
		r.Post("/jobs", jh.Create)
		r.Post("/jobs/bulk", jh.BulkCreate)
		r.Get("/jobs", jh.List)
		r.Get("/jobs/{id}", jh.Get)
		r.Delete("/jobs/{id}", jh.Cancel)

		ph := &handlers.ProcessorHandler{DB: s.opts.DB}
		r.Get("/processors", ph.List)
		r.Get("/processors/{name}", ph.Get)

		wh := &handlers.WebhookHandler{DB: s.opts.DB, Audit: s.opts.Audit}
		r.Post("/webhooks", wh.Create)
		r.Get("/webhooks", wh.List)
		r.Get("/webhooks/{id}", wh.Get)
		r.Patch("/webhooks/{id}", wh.Update)
		r.Delete("/webhooks/{id}", wh.Delete)
		r.Get("/webhooks/{id}/deliveries", wh.ListDeliveries)
		r.Post("/webhooks/{id}/deliveries/{delivery_id}/replay", wh.Replay)

		kh := &handlers.APIKeyHandler{DB: s.opts.DB, Audit: s.opts.Audit}
		r.Post("/api-keys", kh.Create)
		r.Get("/api-keys", kh.List)
		r.Delete("/api-keys/{id}", kh.Revoke)

		sh := &handlers.SystemHandler{DB: s.opts.DB}
		r.Get("/usage", sh.GetUsage)
		r.Get("/audit-log", sh.ListAuditLog)
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
