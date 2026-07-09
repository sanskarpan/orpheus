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
