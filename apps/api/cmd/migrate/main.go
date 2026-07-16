// Command migrate applies the embedded goose migrations to a database.
//
// Usage:
//
//	migrate                       # uses ORPHEUS_TEST_DATABASE_URL, then ORPHEUS_DATABASE_URL
//	migrate postgres://user:pw@host/db?sslmode=disable
//
// It exists so CI (and operators) can bring a fresh database up to the
// current schema in one step, before the test binaries run. Without an
// up-front migration, integration test packages that don't migrate
// themselves race against those that do on a brand-new database.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/orpheus/api/internal/db"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "migrate:", err)
		os.Exit(1)
	}
}

func run() error {
	dsn := ""
	if len(os.Args) > 1 {
		dsn = os.Args[1]
	} else if v := os.Getenv("ORPHEUS_TEST_DATABASE_URL"); v != "" {
		dsn = v
	} else {
		dsn = os.Getenv("ORPHEUS_DATABASE_URL")
	}
	if dsn == "" {
		return fmt.Errorf("no DSN: pass one as an argument or set ORPHEUS_TEST_DATABASE_URL / ORPHEUS_DATABASE_URL")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	defer func() { _ = sqlDB.Close() }()

	if err := db.Migrate(ctx, sqlDB); err != nil {
		return err
	}
	fmt.Println("migrate: schema is up to date")
	return nil
}
