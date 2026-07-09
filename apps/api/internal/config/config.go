// Package config loads runtime configuration from environment variables.
//
// All variables are prefixed with ORPHEUS_, e.g. ORPHEUS_PORT=8080.
package config

import (
	"fmt"

	"github.com/kelseyhightower/envconfig"
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
	return &c, nil
}

// Addr returns the host:port string for the HTTP server.
func (c *Config) Addr() string {
	return fmt.Sprintf("%s:%d", c.Host, c.Port)
}

// IsProd reports whether the service is running in production.
func (c *Config) IsProd() bool {
	return c.Env == "prod"
}
