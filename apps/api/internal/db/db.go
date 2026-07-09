package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DB wraps a pgxpool.Pool so we can hang helpers (such as WithTenant)
// off the connection. The pool itself is promoted so callers can use
// pool methods (Acquire, Query, etc.) directly.
type DB struct {
	*pgxpool.Pool
}

// New opens a pgx connection pool against dsn, pings it, and returns a
// ready-to-use *DB. Pool sizing is a conservative default (2..20 conns);
// tune via config in a later phase when we have real load numbers.
func New(ctx context.Context, dsn string) (*DB, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("db.parse_dsn: %w", err)
	}
	cfg.MaxConns = 20
	cfg.MinConns = 2

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("db.connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("db.ping: %w", err)
	}
	return &DB{Pool: pool}, nil
}

// WithTenant runs fn inside a single-connection transaction with
// `app.current_org_id` set to orgID. All reads and writes through that
// transaction are subject to the RLS policies defined in 0001_init.sql.
//
// Use this for every user-facing request: the auth middleware resolves
// the org from the bearer token, then wraps the handler invocation in
// WithTenant. fn receives the same context it was called with; queries
// performed via the *DB methods on the receiver should use a
// per-request connection (e.g. db.Acquire) to keep the RLS setting in
// scope. A future iteration will thread the pgx.Tx through ctx so
// sqlc-generated queries inside fn run on the same transaction.
func (db *DB) WithTenant(ctx context.Context, orgID string, fn func(ctx context.Context) error) error {
	conn, err := db.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("db.with_tenant.acquire: %w", err)
	}
	defer conn.Release()

	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("db.with_tenant.begin: %w", err)
	}
	// Rollback is a no-op if Commit succeeded; errors are intentionally
	// swallowed because the only failure mode here is "tx already
	// finalized", which is benign.
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, "SET LOCAL app.current_org_id = $1", orgID); err != nil {
		return fmt.Errorf("db.with_tenant.set_org: %w", err)
	}

	if err := fn(ctx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("db.with_tenant.commit: %w", err)
	}
	return nil
}

// Compile-time check: pgx.Tx is what the transaction object from Begin
// actually is, so sqlc-generated code (which takes pgx.Tx) can be
// satisfied by a handle pulled out of context once we wire that up.
var _ pgx.Tx
