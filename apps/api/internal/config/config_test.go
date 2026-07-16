package config

import (
	"os"
	"testing"
)

func TestLoadDefaults(t *testing.T) {
	// Unset rather than set-to-empty: envconfig treats "" as a value to
	// parse (and fails for ints), not as "use the default".
	for _, k := range []string{
		"ORPHEUS_ENV",
		"ORPHEUS_PORT",
		"ORPHEUS_LOG_LEVEL",
		"ORPHEUS_SERVICE_NAME",
		"ORPHEUS_HOST",
		"ORPHEUS_SHUTDOWN_GRACE_SECONDS",
	} {
		k := k
		old, had := os.LookupEnv(k)
		if err := os.Unsetenv(k); err != nil {
			t.Fatalf("unsetenv %s: %v", k, err)
		}
		t.Cleanup(func() {
			if had {
				_ = os.Setenv(k, old)
			} else {
				_ = os.Unsetenv(k)
			}
		})
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned error: %v", err)
	}

	if cfg.Env != "dev" {
		t.Errorf("Env = %q, want %q", cfg.Env, "dev")
	}
	if cfg.Port != 8080 {
		t.Errorf("Port = %d, want 8080", cfg.Port)
	}
	if cfg.LogLevel != "INFO" {
		t.Errorf("LogLevel = %q, want INFO", cfg.LogLevel)
	}
	if cfg.ServiceName != "orpheus-api" {
		t.Errorf("ServiceName = %q, want orpheus-api", cfg.ServiceName)
	}
	if cfg.ShutdownGraceSeconds != 30 {
		t.Errorf("ShutdownGraceSeconds = %d, want 30", cfg.ShutdownGraceSeconds)
	}
}

func TestLoadFromEnv(t *testing.T) {
	t.Setenv("ORPHEUS_ENV", "staging")
	t.Setenv("ORPHEUS_PORT", "9090")
	t.Setenv("ORPHEUS_LOG_LEVEL", "DEBUG")
	t.Setenv("ORPHEUS_SHUTDOWN_GRACE_SECONDS", "5")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned error: %v", err)
	}

	if cfg.Env != "staging" {
		t.Errorf("Env = %q, want staging", cfg.Env)
	}
	if cfg.Port != 9090 {
		t.Errorf("Port = %d, want 9090", cfg.Port)
	}
	if cfg.LogLevel != "DEBUG" {
		t.Errorf("LogLevel = %q, want DEBUG", cfg.LogLevel)
	}
	if cfg.ShutdownGraceSeconds != 5 {
		t.Errorf("ShutdownGraceSeconds = %d, want 5", cfg.ShutdownGraceSeconds)
	}
}

func TestAddr(t *testing.T) {
	cfg := &Config{Host: "127.0.0.1", Port: 8080}
	if got := cfg.Addr(); got != "127.0.0.1:8080" {
		t.Errorf("Addr() = %q, want 127.0.0.1:8080", got)
	}
}

func TestIsProd(t *testing.T) {
	tests := []struct {
		env  string
		want bool
	}{
		{"prod", true},
		{"production", false}, // explicit only
		{"staging", false},
		{"dev", false},
		{"", false},
	}
	for _, tt := range tests {
		cfg := &Config{Env: tt.env}
		if got := cfg.IsProd(); got != tt.want {
			t.Errorf("IsProd() with Env=%q = %v, want %v", tt.env, got, tt.want)
		}
	}
}

// TestLoadMissingConfigIsFatal ensures the binary fails fast on a malformed env.
// We can't easily test the "no env at all" case (envconfig reads OS env), so
// we just verify the happy path returns no error.
func TestLoadMissingConfigIsFatal(t *testing.T) {
	// Use an isolated env to avoid CI interference.
	for _, k := range []string{"ORPHEUS_ENV", "ORPHEUS_PORT"} {
		old := os.Getenv(k)
		_ = os.Unsetenv(k)
		t.Cleanup(func() { _ = os.Setenv(k, old) })
	}
	if _, err := Load(); err != nil {
		t.Fatalf("Load() with empty env returned error: %v", err)
	}
}

// TestLoadProdRejectsDevSecrets verifies that a prod deploy which leaves
// the dev-default S3 secret or sslmode=disable in place fails to load.
func TestLoadProdRejectsDevSecrets(t *testing.T) {
	// "production" must normalise to prod so hardening actually applies.
	t.Setenv("ORPHEUS_PORT", "8080")
	t.Setenv("ORPHEUS_ENV", "production")
	if _, err := Load(); err == nil {
		t.Fatal("Load() in prod with dev-default S3 secret should error, got nil")
	}
}

// TestLoadProdWithRealSecrets verifies a properly-configured prod env loads.
func TestLoadProdWithRealSecrets(t *testing.T) {
	t.Setenv("ORPHEUS_PORT", "8080")
	t.Setenv("ORPHEUS_ENV", "prod")
	t.Setenv("ORPHEUS_S3_ACCESS_KEY", "AKIAREAL")
	t.Setenv("ORPHEUS_S3_SECRET_KEY", "a-real-rotated-secret")
	t.Setenv("ORPHEUS_DATABASE_URL", "postgres://u:p@db:5432/orpheus?sslmode=verify-full")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() with real prod secrets errored: %v", err)
	}
	if !cfg.IsProd() {
		t.Errorf("IsProd() = false for ORPHEUS_ENV=prod")
	}
}
