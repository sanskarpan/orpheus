// Package db owns the database connection pool, the embedded goose
// migration runner, and the generated sqlc queries.
//
// All public surface is in package db; subpackages (queries, migrations)
// are internal to the package and not meant to be imported directly.
package db

import (
	"context"
	"database/sql"
	"embed"
	"fmt"

	"github.com/pressly/goose/v3"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Migrate runs every goose-formatted SQL file in the embedded migrations
// directory against db. Idempotent: subsequent calls after a successful
// run are no-ops. Migrations are tracked in the `goose_db_version` table.
//
// The function is safe to call at process start before opening the
// long-lived pgx pool used by the request path; it uses database/sql
// because goose v3 still targets that interface for its driver-agnostic
// migration runner.
func Migrate(ctx context.Context, db *sql.DB) error {
	goose.SetBaseFS(migrationsFS)
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("db.migrate.set_dialect: %w", err)
	}
	if err := goose.UpContext(ctx, db, "migrations"); err != nil {
		return fmt.Errorf("db.migrate.up: %w", err)
	}
	return nil
}
