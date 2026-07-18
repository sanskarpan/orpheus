// Command canary is a standalone synthetic prober for the Orpheus API.
//
// It probes the API's /health and /ready endpoints every interval and exposes
// the results as Prometheus metrics (orpheus_canary_up, ..._probes_total,
// ..._probe_duration_seconds) on its own /metrics endpoint, so Prometheus can
// alert on a client-facing outage the API's self-scraped metrics can't see.
//
// Env:
//
//	ORPHEUS_CANARY_TARGET_URL       (default http://localhost:8080)
//	ORPHEUS_CANARY_INTERVAL_SECONDS (default 60; roadmap asks for ~5 min)
//	ORPHEUS_CANARY_METRICS_PORT     (default 9102)
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/orpheus/api/internal/canary"
)

func main() {
	target := envOr("ORPHEUS_CANARY_TARGET_URL", "http://localhost:8080")
	interval := time.Duration(envInt("ORPHEUS_CANARY_INTERVAL_SECONDS", 60)) * time.Second
	port := envOr("ORPHEUS_CANARY_METRICS_PORT", "9102")
	logger := slog.Default()

	reg := prometheus.NewRegistry()
	prober := canary.New(target, reg)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	srv := &http.Server{Addr: ":" + port, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("canary.metrics_server", "err", err)
		}
	}()
	logger.Info("canary.started", "target", target, "interval", interval.String(), "metrics_port", port)

	prober.Run(ctx, interval)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	logger.Info("canary.stopped")
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}
