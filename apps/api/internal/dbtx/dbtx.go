// Package dbtx owns the context-keyed transaction handle used to
// thread the WithTenant pgx.Tx through request scopes. It also
// exposes a Querier interface and Exec/Query/QueryRow helpers that
// run on the tx attached to ctx when one is present, and fall back
// to a passed-in Querier (typically *pgxpool.Pool) when there is no
// tx in ctx.
//
// The why: WithTenant sets the row-level-security guard
// `app.current_org_id` on a *transaction* connection. Queries that
// run on a *different* connection from the pool bypass the GUC and
// therefore RLS, leaving the de facto `WHERE org_id = $1` clause on
// every handler query as the only thing standing between two tenants.
// That is fragile. By making handlers call dbtx.Exec/Query/QueryRow
// with the ctx WithTenant provides, the same connection carries the
// GUC and the query, and RLS becomes load-bearing.
//
// The fallback is intentional: handler code that runs outside a
// WithTenant scope (background workers, system reads, migrations,
// the processor catalog lookup) can still call dbtx.Exec(ctx, pool,
// ...) and have it land on the pool. The helpers are safe to use
// everywhere; the difference is that within WithTenant the query
// becomes RLS-scoped automatically.
package dbtx

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type ctxKey struct{}

// WithTx returns a new ctx with tx attached. Callers should not
// retain the returned ctx past the lifetime of tx.
func WithTx(ctx context.Context, tx pgx.Tx) context.Context {
	return context.WithValue(ctx, ctxKey{}, tx)
}

// FromContext returns the tx attached to ctx, or nil when there is
// none. A nil return is the signal to fall back to the pool.
func FromContext(ctx context.Context) pgx.Tx {
	tx, _ := ctx.Value(ctxKey{}).(pgx.Tx)
	return tx
}

// Querier is the subset of pgx operations the helpers need. Both
// *pgxpool.Pool and pgx.Tx satisfy it.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Exec runs sql on the tx attached to ctx, falling back to fallback
// when no tx is present. See the package comment for the rationale.
func Exec(ctx context.Context, fallback Querier, sql string, args ...any) (pgconn.CommandTag, error) {
	if tx := FromContext(ctx); tx != nil {
		return tx.Exec(ctx, sql, args...)
	}
	return fallback.Exec(ctx, sql, args...)
}

// Query is the Query counterpart to Exec.
func Query(ctx context.Context, fallback Querier, sql string, args ...any) (pgx.Rows, error) {
	if tx := FromContext(ctx); tx != nil {
		return tx.Query(ctx, sql, args...)
	}
	return fallback.Query(ctx, sql, args...)
}

// QueryRow is the QueryRow counterpart to Exec.
func QueryRow(ctx context.Context, fallback Querier, sql string, args ...any) pgx.Row {
	if tx := FromContext(ctx); tx != nil {
		return tx.QueryRow(ctx, sql, args...)
	}
	return fallback.QueryRow(ctx, sql, args...)
}
