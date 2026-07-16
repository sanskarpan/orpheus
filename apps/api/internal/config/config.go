// Package config loads runtime configuration from environment variables.
//
// All variables are prefixed with ORPHEUS_, e.g. ORPHEUS_PORT=8080.
package config

import (
	"fmt"
	"strings"

	"github.com/kelseyhightower/envconfig"
)

// Dev-only default credentials. In production these must be overridden;
// Load() refuses to start if any survive into a prod environment.
const (
	devS3AccessKey = "orpheus"
	devS3SecretKey = "orpheus-dev-secret"
)

// Config holds the runtime configuration of the API binary.
type Config struct {
	// Environment: dev, staging, or prod. Affects log format and debug mode.
	Env string `envconfig:"ENV" default:"dev"`

	// LogLevel: DEBUG, INFO, WARN, ERROR.
	LogLevel string `envconfig:"LOG_LEVEL" default:"INFO"`

	// ServiceName identifies the service in logs and metrics.
	ServiceName string `envconfig:"SERVICE_NAME" default:"orpheus-api"`

	// HTTP server bind address and port.
	Host string `envconfig:"HOST" default:"0.0.0.0"`
	Port int    `envconfig:"PORT" default:"8080"`

	// ShutdownGraceSeconds is how long the server waits for in-flight
	// requests to complete before forcefully shutting down.
	ShutdownGraceSeconds int `envconfig:"SHUTDOWN_GRACE_SECONDS" default:"30"`

	// --- External services (Phase 1+) ------------------------------------
	// DatabaseURL points at the Postgres primary. The same value is
	// passed to both the sql.DB used by goose migrations and the pgx
	// pool used at request time.
	DatabaseURL string `envconfig:"DATABASE_URL" default:"postgres://orpheus:orpheus@localhost:5432/orpheus?sslmode=disable"`

	// RedisURL is used for the Arq work queue, rate limiting, and
	// short-lived idempotency cache.
	RedisURL string `envconfig:"REDIS_URL" default:"redis://localhost:6379/0"`

	// NATSURL is the JetStream connection string used by the outbox
	// dispatcher and the worker's job-channel subscriber.
	NATSURL string `envconfig:"NATS_URL" default:"nats://localhost:4222"`

	// S3Endpoint is the MinIO endpoint in dev. In prod this is the
	// regional S3 endpoint (e.g. https://s3.us-east-1.amazonaws.com).
	S3Endpoint  string `envconfig:"S3_ENDPOINT" default:"http://localhost:9000"`
	S3AccessKey string `envconfig:"S3_ACCESS_KEY" default:"orpheus"`
	S3SecretKey string `envconfig:"S3_SECRET_KEY" default:"orpheus-dev-secret"`

	// S3Bucket is the bucket where upload parts and finalized artifacts
	// live. One bucket per environment; key prefix encodes the org id.
	S3Bucket string `envconfig:"S3_BUCKET" default:"orpheus-uploads"`

	// RateLimitFailClosed, when true, makes the rate-limit middleware
	// reject requests with 503 if the Redis backend errors (instead of
	// failing open). It is forced on in prod regardless of this value.
	RateLimitFailClosed bool `envconfig:"RATE_LIMIT_FAIL_CLOSED" default:"false"`

	// Keycloak is the OIDC provider. The API validates bearer tokens
	// against this realm; clients obtain tokens out-of-band.
	KeycloakURL      string `envconfig:"KEYCLOAK_URL" default:"http://localhost:8088"`
	KeycloakRealm    string `envconfig:"KEYCLOAK_REALM" default:"orpheus"`
	KeycloakClientID string `envconfig:"KEYCLOAK_CLIENT_ID" default:"orpheus-api"`
}

// Load reads the configuration from the environment.
// It panics on parse error because the binary cannot run safely without
// a valid configuration; the panic is caught by the process supervisor.
func Load() (*Config, error) {
	var c Config
	if err := envconfig.Process("ORPHEUS", &c); err != nil {
		return nil, fmt.Errorf("orpheus_api.config.load: %w", err)
	}
	c.Env = normalizeEnv(c.Env)
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("orpheus_api.config.validate: %w", err)
	}
	return &c, nil
}

// normalizeEnv lowercases the environment and folds "production" into
// "prod" so security-relevant branches (see IsProd) are not silently
// disabled by a spelling difference.
func normalizeEnv(env string) string {
	switch strings.ToLower(strings.TrimSpace(env)) {
	case "prod", "production":
		return "prod"
	case "staging", "stage":
		return "staging"
	default:
		return "dev"
	}
}

// validate rejects insecure configurations. In production it refuses the
// dev-default S3 credentials and a TLS-disabled database connection so a
// deploy that forgets to set a secret fails loudly instead of silently
// running with a well-known key over an unencrypted link.
func (c *Config) validate() error {
	if !c.IsProd() {
		return nil
	}
	var problems []string
	if c.S3AccessKey == devS3AccessKey {
		problems = append(problems, "ORPHEUS_S3_ACCESS_KEY is the dev default")
	}
	if c.S3SecretKey == devS3SecretKey {
		problems = append(problems, "ORPHEUS_S3_SECRET_KEY is the dev default")
	}
	if strings.Contains(c.DatabaseURL, "sslmode=disable") {
		problems = append(problems, "ORPHEUS_DATABASE_URL has sslmode=disable")
	}
	if len(problems) > 0 {
		return fmt.Errorf("insecure prod config: %s", strings.Join(problems, "; "))
	}
	return nil
}

// Addr returns the host:port string for the HTTP server.
func (c *Config) Addr() string {
	return fmt.Sprintf("%s:%d", c.Host, c.Port)
}

// IsProd reports whether the service is running in production.
func (c *Config) IsProd() bool {
	return c.Env == "prod"
}
