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

	"github.com/orpheus/api/internal/config"
	"github.com/orpheus/api/internal/handlers"
)

// Server is the HTTP server for the Orpheus API.
//
// The zero value is NOT usable; construct via [New].
type Server struct {
	cfg    *config.Config
	logger *slog.Logger
	mux    *chi.Mux
	http   *http.Server
}

// New constructs a Server with routes wired and middleware applied.
// It does not start listening; call [Server.Run] for that.
func New(cfg *config.Config, logger *slog.Logger) *Server {
	s := &Server{
		cfg:    cfg,
		logger: logger,
		mux:    chi.NewRouter(),
	}
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

// routes installs middleware and registers every HTTP route.
//
// Middleware order matters:
//   - RequestID: assigns an ID to the request and adds it to the context,
//     so later middleware / handlers / logs can correlate.
//   - Recoverer: turns panics into 500 responses instead of crashing the
//     process.
//   - Timeout: caps each handler at 30s. Slowloris-style attacks cannot
//     hold a connection open indefinitely.
//
// Note: middleware.RealIP is intentionally omitted. It was deprecated in
// chi v5.3.1 (GHSA-3fxj-6jh8-hvhx) because it trusts X-Forwarded-For
// without verifying the request came from a trusted proxy. Phase 1+ will
// add a vetted client-IP helper once a load balancer is in front of the
// service.
func (s *Server) routes() {
	s.mux.Use(middleware.RequestID)
	s.mux.Use(middleware.Recoverer)
	s.mux.Use(middleware.Timeout(30 * time.Second))

	s.mux.Get("/health", handlers.Liveness)
	s.mux.Get("/ready", handlers.Readiness)
	s.mux.Get("/metrics", handlers.Metrics)

	s.mux.Route("/api", func(r chi.Router) {
		r.Get("/openapi.json", handlers.OpenAPISpec)
		r.Get("/docs", handlers.SwaggerUI)
		r.Get("/redoc", handlers.ReDocUI)
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
