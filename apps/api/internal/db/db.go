package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/orpheus/api/internal/dbtx"
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
// `app.current_org_id` set to orgID. The tx is attached to the ctx
// passed to fn via dbtx.WithTx; handlers pull it out through the
// dbtx.Exec/Query/QueryRow helpers (or the raw tx from
// dbtx.FromContext) so every query inside fn runs on the same
// connection that holds the GUC. That makes the row-level-security
// policies defined in 0001_init.sql load-bearing instead of advisory.
//
// Trade-off: the dbtx helpers fall back to the pool when no tx is
// present, so handler code that legitimately runs outside WithTenant
// (background workers, system reads, the processor catalog lookup)
// keeps working unchanged. Inside WithTenant, calls to
// h.DB.Exec/Query/QueryRow (pool methods) silently bypass the GUC
// because they acquire a *different* connection from the pool — use
// the dbtx helpers instead.
//
// Use this for every user-facing request: the auth middleware resolves
// the org from the bearer token, then wraps the handler invocation in
// WithTenant.
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

	// set_config() (a function call) accepts a parameter; the
	// "SET LOCAL ... = $1" form is invalid because SET does not
	// bind parameters. The third arg, `true`, scopes the setting
	// to the current transaction, matching SET LOCAL semantics.
	if _, err := tx.Exec(ctx, "SELECT set_config('app.current_org_id', $1, true)", orgID); err != nil {
		return fmt.Errorf("db.with_tenant.set_org: %w", err)
	}

	txCtx := dbtx.WithTx(ctx, tx)
	if err := fn(txCtx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("db.with_tenant.commit: %w", err)
	}
	return nil
}

// Compile-time checks: pgx.Tx is what the transaction object from Begin
// actually is, so sqlc-generated code (which takes pgx.Tx) can be
// satisfied by a handle pulled out of context once we wire that up.
// *pgxpool.Pool satisfies the dbtx.Querier interface, so the dbtx
// helpers can fall back to the pool when no tx is present in ctx.
var (
	_ pgx.Tx
	_ dbtx.Querier = (*pgxpool.Pool)(nil)
)
