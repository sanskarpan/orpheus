// Command api is the entry point for the Orpheus HTTP API.
//
// It loads configuration from the environment, configures structured
// logging, builds the HTTP server, and blocks until SIGINT or SIGTERM,
// at which point it triggers a graceful shutdown bounded by
// ORPHEUS_SHUTDOWN_GRACE_SECONDS.
package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/orpheus/api/internal/config"
	"github.com/orpheus/api/internal/logging"
	"github.com/orpheus/api/internal/server"
	"github.com/orpheus/api/internal/version"
)

func main() {
	if err := run(); err != nil {
		slog.Error("orpheus_api.fatal", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	logging.Configure(cfg.LogLevel, cfg.IsProd())
	logger := slog.Default().With("service", cfg.ServiceName)

	logger.Info("orpheus_api.starting",
		"version", version.Version,
		"env", cfg.Env,
		"log_level", cfg.LogLevel,
	)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	srv := server.New(cfg, logger)
	if err := srv.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}

	logger.Info("orpheus_api.stopped",
		"version", version.Version,
		"env", cfg.Env,
	)
	return nil
}
