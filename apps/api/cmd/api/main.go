// Command api is the entry point for the Orpheus HTTP API.
//
// It loads configuration from the environment, configures structured
// logging, runs database migrations, constructs the full service
// graph (DB pool, S3, auth, idempotency, rate limit, audit, outbox,
// webhook delivery), and blocks until SIGINT or SIGTERM, at which
// point it triggers a graceful shutdown bounded by
// ORPHEUS_SHUTDOWN_GRACE_SECONDS.
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/redis/go-redis/v9"

	"github.com/orpheus/api/internal/audit"
	"github.com/orpheus/api/internal/auth"
	"github.com/orpheus/api/internal/billing"
	"github.com/orpheus/api/internal/config"
	"github.com/orpheus/api/internal/db"
	"github.com/orpheus/api/internal/idempotency"
	"github.com/orpheus/api/internal/jobs"
	"github.com/orpheus/api/internal/logging"
	"github.com/orpheus/api/internal/metrics"
	"github.com/orpheus/api/internal/observability"
	"github.com/orpheus/api/internal/outbox"
	"github.com/orpheus/api/internal/ratelimit"
	"github.com/orpheus/api/internal/retention"
	"github.com/orpheus/api/internal/server"
	"github.com/orpheus/api/internal/storage/s3"
	"github.com/orpheus/api/internal/version"
	"github.com/orpheus/api/internal/webhooks"
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

	otelShutdown, err := observability.Init(ctx)
	if err != nil {
		return fmt.Errorf("orpheus_api.tracing_init: %w", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := otelShutdown(shutdownCtx); err != nil {
			logger.Warn("orpheus_api.tracing_shutdown_failed", "err", err)
		}
	}()

	// Migrations first. We open a short-lived database/sql connection
	// against the same DSN the pool will use, run every embedded goose
	// migration, and close the handle. A failure here is fatal — a
	// process running with an out-of-date schema would corrupt user
	// data on the first write.
	if err := runMigrations(ctx, cfg, logger); err != nil {
		return fmt.Errorf("orpheus_api.migrate: %w", err)
	}

	// Build the long-lived pgx pool used by the request path.
	pgDB, err := db.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("orpheus_api.db_open: %w", err)
	}
	defer pgDB.Close()

	// In prod, refuse to run as a role that bypasses RLS — that would
	// silently defeat tenant isolation regardless of FORCE policies.
	if cfg.IsProd() {
		if err := pgDB.AssertTenantSafeRole(ctx); err != nil {
			return fmt.Errorf("orpheus_api.db_role_unsafe: %w", err)
		}
	}

	// S3 client. Used by uploads to mint presigned URLs and by
	// webhook delivery to fetch the object body (when the worker
	// starts pulling payloads from S3 in Phase 2).
	s3c, err := s3.New(ctx, cfg)
	if err != nil {
		return fmt.Errorf("orpheus_api.s3_open: %w", err)
	}

	// Auth stack. Both verifiers share the same pool; either may be
	// nil in dev (a config without Keycloak still accepts API keys,
	// and vice versa) — the authenticator reports a clear error when
	// the request uses a disabled method.
	keycloak, err := auth.NewKeycloakVerifier(ctx, cfg)
	if err != nil {
		// Keycloak unreachable at startup is a warning, not a fatal
		// error: the JWKS cache will retry on the next request.
		logger.Warn("orpheus_api.keycloak_unavailable", "err", err)
	}
	apikey := auth.NewAPIKeyValidator(pgDB)
	authn := &auth.Authenticator{Keycloak: keycloak, APIKey: apikey}

	// Cross-cutting middleware. Each is optional; the server treats
	// nil fields as "feature disabled" so a minimal binary can omit
	// Redis or the audit recorder.
	auditRec := audit.New(pgDB, logger)
	idempMW := idempotency.New(pgDB)

	var rateMW *ratelimit.Middleware
	var rdb *redis.Client
	if rdb, err = openRedis(ctx, cfg.RedisURL); err != nil {
		logger.Warn("orpheus_api.redis_unavailable", "err", err, "note", "rate limiting disabled")
	} else {
		defer func() { _ = rdb.Close() }()
		rateMW = ratelimit.NewMiddleware(ratelimit.New(rdb), logger)
		// In prod, a limiter backend outage must not become a bypass.
		rateMW.FailClosed = cfg.RateLimitFailClosed || cfg.IsProd()
	}

	// NATS connection. The outbox publisher drains DB rows to the
	// ORPHEUS_JOBS JetStream stream; the webhook delivery service
	// subscribes to `adkil.>` for the fast enqueue path. Both are
	// safe to run with a nil conn — they fall back to poll-only
	// behaviour — so a dev binary without NATS still serves traffic.
	natsConn, err := openNATS(ctx, cfg.NATSURL)
	if err != nil {
		logger.Warn("orpheus_api.nats_unavailable", "err", err, "note", "running in poll-only mode")
	} else {
		defer natsConn.Close()
	}

	// JetStream context. nil when the NATS connection is nil or
	// when the server has JetStream disabled; either way the
	// outbox publisher falls back to "skip publish" with a warning
	// on the first attempt, matching the soft-dep pattern used for
	// Redis and Keycloak.
	var js jetstream.JetStream
	if natsConn != nil {
		js, err = jetstream.New(natsConn)
		if err != nil {
			logger.Warn("orpheus_api.jetstream_unavailable", "err", err, "note", "outbox publisher will skip publishes")
		} else if err := jobs.EnsureStream(js, jobs.DefaultRetentionDays); err != nil {
			logger.Warn("orpheus_api.jetstream_ensure_stream_failed", "err", err)
		}
	}

	// Background workers. Each is a long-running goroutine that exits
	// when ctx is cancelled. The wait group lets run() block until
	// every worker has returned, so no in-flight delivery is lost at
	// shutdown.
	mtr := metrics.New()
	publisher := outbox.New(pgDB, js, mtr, logger)
	delivery := webhooks.New(pgDB, logger, natsConn, nil)
	sweeper := retention.New(pgDB, s3c, logger)
	rollup := billing.NewRollup(pgDB, logger)

	// Payment provider. Real Dodo integration when an API key is present;
	// otherwise nil so the checkout endpoint reports "not configured" and
	// the inbound webhook route is not mounted. The usage rollup runs
	// regardless so invoices accrue even before payments are wired up.
	var billingProvider billing.Provider
	if cfg.DodoAPIKey != "" {
		billingProvider = billing.NewDodoProvider(cfg.DodoAPIKey, cfg.DodoWebhookSecret, cfg.DodoBaseURL)
		logger.Info("orpheus_api.billing.provider", "provider", billingProvider.Name())
	} else {
		logger.Warn("orpheus_api.billing.provider_unset", "note", "checkout disabled; set ORPHEUS_DODO_API_KEY to enable")
	}

	var workers sync.WaitGroup
	startWorker(ctx, &workers, "outbox.publisher", publisher.Run)
	startWorker(ctx, &workers, "webhooks.delivery", delivery.Run)
	startWorker(ctx, &workers, "retention.sweeper", sweeper.Run)
	startWorker(ctx, &workers, "billing.rollup", rollup.Run)

	srv := server.NewWithOptions(cfg, logger, server.Options{
		DB:          pgDB,
		S3:          s3c,
		Authn:       authn,
		Idempotency: idempMW,
		RateLimit:   rateMW,
		Audit:       auditRec,
		Metrics:     mtr,
		Billing:     billingProvider,
	})

	logger.Info("orpheus_api.ready", "addr", cfg.Addr())
	if err := srv.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("orpheus_api.server: %w", err)
	}

	// Server has stopped accepting new requests. Wait for the
	// background workers to drain their current iteration before we
	// return, so an at-least-once delivery isn't lost.
	waitWorkers(&workers, cfg.ShutdownGraceSeconds, logger)

	logger.Info("orpheus_api.stopped",
		"version", version.Version,
		"env", cfg.Env,
	)
	return nil
}

// runMigrations opens a short-lived database/sql connection against
// the same DSN the pool uses and applies every embedded migration.
// The handle is closed before the function returns, so a deploy that
// crashes mid-migration leaves the cluster with whatever the prior
// goose version recorded.
func runMigrations(ctx context.Context, cfg *config.Config, logger *slog.Logger) error {
	sqlDB, err := sql.Open("pgx", cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("sql.open: %w", err)
	}
	defer func() { _ = sqlDB.Close() }()

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := sqlDB.PingContext(pingCtx); err != nil {
		return fmt.Errorf("sql.ping: %w", err)
	}

	logger.Info("orpheus_api.migrations.starting")
	if err := db.Migrate(ctx, sqlDB); err != nil {
		return err
	}
	logger.Info("orpheus_api.migrations.applied")
	return nil
}

// openRedis parses a redis:// URL and returns a *redis.Client. We
// rewrite the scheme because go-redis expects an addr ("host:port"),
// not a URL. A failure here is non-fatal: the caller logs and
// continues with rate limiting disabled.
func openRedis(ctx context.Context, rawURL string) (*redis.Client, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("redis.parse: %w", err)
	}
	addr := u.Host
	if u.User != nil {
		addr = u.User.Username() + "@" + addr
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := rdb.Ping(pingCtx).Err(); err != nil {
		_ = rdb.Close()
		return nil, fmt.Errorf("redis.ping: %w", err)
	}
	return rdb, nil
}

// openNATS connects to the configured NATS URL. The outbox
// publisher uses the JetStream view of the same connection; the
// webhook delivery service uses core NATS for the adkil.>
// fast-enqueue subscription. A failure here is non-fatal: both
// services fall back to poll-only behaviour.
func openNATS(_ context.Context, rawURL string) (*nats.Conn, error) {
	conn, err := nats.Connect(rawURL,
		nats.Name(strings.TrimSpace("orpheus-api")),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(2*time.Second),
	)
	if err != nil {
		return nil, fmt.Errorf("nats.connect: %w", err)
	}
	return conn, nil
}

// startWorker launches fn in a goroutine and tracks it in wg. The
// worker is expected to block until ctx is cancelled. A nil fn is
// skipped (no-op) so a partially-initialised binary can still boot.
func startWorker(ctx context.Context, wg *sync.WaitGroup, name string, fn func(context.Context) error) {
	if fn == nil {
		return
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := fn(ctx); err != nil {
			slog.Error("orpheus_api.worker_exited", "worker", name, "err", err)
		}
	}()
}

// waitWorkers blocks until every tracked worker has returned or
// graceSeconds elapses, whichever comes first. A worker that doesn't
// return inside the grace window is logged and abandoned — the
// alternative (blocking forever) is worse for the operator trying to
// roll a new release.
func waitWorkers(wg *sync.WaitGroup, graceSeconds int, logger *slog.Logger) {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return
	case <-time.After(time.Duration(graceSeconds) * time.Second):
		logger.Warn("orpheus_api.workers_drain_timeout", "grace_seconds", graceSeconds)
	}
}
