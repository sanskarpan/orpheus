package dbtx

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// stubQuerier records the call site (which method was invoked and
// with which ctx) so tests can assert which of {tx, fallback} the
// helper actually used. Each method is a no-op that returns the
// pre-baked values the test set up.
type stubQuerier struct {
	execCalls  int
	queryCalls int
	rowCalls   int
	lastCtx    context.Context
	execTag    pgconn.CommandTag
	execErr    error
	queryErr   error
	scanErr    error
}

func (s *stubQuerier) Exec(ctx context.Context, _ string, _ ...any) (pgconn.CommandTag, error) {
	s.execCalls++
	s.lastCtx = ctx
	return s.execTag, s.execErr
}

func (s *stubQuerier) Query(ctx context.Context, _ string, _ ...any) (pgx.Rows, error) {
	s.queryCalls++
	s.lastCtx = ctx
	return nil, s.queryErr
}

func (s *stubQuerier) QueryRow(ctx context.Context, _ string, _ ...any) pgx.Row {
	s.rowCalls++
	s.lastCtx = ctx
	return stubRow{err: s.scanErr}
}

type stubRow struct{ err error }

func (r stubRow) Scan(_ ...any) error { return r.err }

// fakeTx is a minimal pgx.Tx that records whether its methods were
// called. We use it to verify WithTx round-trips the same handle
// back through FromContext.
type fakeTx struct {
	execCalls  int
	queryCalls int
	rowCalls   int
}

func (t *fakeTx) Begin(_ context.Context) (pgx.Tx, error) { return nil, nil }
func (t *fakeTx) Commit(_ context.Context) error          { return nil }
func (t *fakeTx) Rollback(_ context.Context) error        { return nil }
func (t *fakeTx) CopyFrom(_ context.Context, _ pgx.Identifier, _ []string, _ pgx.CopyFromSource) (int64, error) {
	return 0, nil
}
func (t *fakeTx) SendBatch(_ context.Context, _ *pgx.Batch) pgx.BatchResults { return nil }
func (t *fakeTx) LargeObjects() pgx.LargeObjects                             { return pgx.LargeObjects{} }
func (t *fakeTx) Prepare(_ context.Context, _, _ string) (*pgconn.StatementDescription, error) {
	return nil, nil
}
func (t *fakeTx) Exec(_ context.Context, _ string, _ ...any) (pgconn.CommandTag, error) {
	t.execCalls++
	return pgconn.NewCommandTag(""), nil
}
func (t *fakeTx) Query(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
	t.queryCalls++
	return nil, errors.New("fakeTx.Query: not used in these tests")
}
func (t *fakeTx) QueryRow(_ context.Context, _ string, _ ...any) pgx.Row {
	t.rowCalls++
	return stubRow{err: nil}
}
func (t *fakeTx) Conn() *pgx.Conn { return nil }

func TestWithTx_RoundTrip(t *testing.T) {
	tx := &fakeTx{}
	ctx := WithTx(context.Background(), tx)
	if got := FromContext(ctx); got != tx {
		t.Fatalf("FromContext returned %v, want the same fakeTx handle", got)
	}
}

func TestFromContext_Nil(t *testing.T) {
	if got := FromContext(context.Background()); got != nil {
		t.Fatalf("FromContext on plain ctx returned %v, want nil", got)
	}
}

func TestExec_UsesTxWhenSet(t *testing.T) {
	tx := &fakeTx{}
	fb := &stubQuerier{}
	ctx := WithTx(context.Background(), tx)

	if _, err := Exec(ctx, fb, "select 1"); err != nil {
		t.Fatalf("Exec returned unexpected error: %v", err)
	}
	if tx.execCalls != 1 {
		t.Errorf("tx.Exec calls = %d, want 1", tx.execCalls)
	}
	if fb.execCalls != 0 {
		t.Errorf("fallback.Exec calls = %d, want 0 (tx should have handled it)", fb.execCalls)
	}
}

func TestExec_FallsBackToPool(t *testing.T) {
	fb := &stubQuerier{execTag: pgconn.NewCommandTag("SELECT 1")}
	if _, err := Exec(context.Background(), fb, "select 1"); err != nil {
		t.Fatalf("Exec returned unexpected error: %v", err)
	}
	if fb.execCalls != 1 {
		t.Errorf("fallback.Exec calls = %d, want 1", fb.execCalls)
	}
}

func TestQuery_UsesTxWhenSet(t *testing.T) {
	tx := &fakeTx{}
	fb := &stubQuerier{}
	ctx := WithTx(context.Background(), tx)

	// fakeTx.Query is wired to error; that error is what the helper
	// propagates. The point of this test is just that the call lands
	// on tx and not on fb.
	_, err := Query(ctx, fb, "select 1")
	if err == nil {
		t.Fatalf("Query returned nil error; expected fakeTx.Query sentinel")
	}
	if tx.queryCalls != 1 {
		t.Errorf("tx.Query calls = %d, want 1", tx.queryCalls)
	}
	if fb.queryCalls != 0 {
		t.Errorf("fallback.Query calls = %d, want 0", fb.queryCalls)
	}
}

func TestQuery_FallsBackToPool(t *testing.T) {
	fb := &stubQuerier{queryErr: errors.New("fallback path used")}
	if _, err := Query(context.Background(), fb, "select 1"); err == nil || err.Error() != "fallback path used" {
		t.Fatalf("Query err = %v, want fallback path sentinel", err)
	}
	if fb.queryCalls != 1 {
		t.Errorf("fallback.Query calls = %d, want 1", fb.queryCalls)
	}
}

func TestQueryRow_UsesTxWhenSet(t *testing.T) {
	tx := &fakeTx{}
	fb := &stubQuerier{}
	ctx := WithTx(context.Background(), tx)

	if err := QueryRow(ctx, fb, "select 1").Scan(nil); err != nil {
		t.Fatalf("QueryRow.Scan returned unexpected error: %v", err)
	}
	if tx.rowCalls != 1 {
		t.Errorf("tx.QueryRow calls = %d, want 1", tx.rowCalls)
	}
	if fb.rowCalls != 0 {
		t.Errorf("fallback.QueryRow calls = %d, want 0", fb.rowCalls)
	}
}

func TestQueryRow_FallsBackToPool(t *testing.T) {
	fb := &stubQuerier{scanErr: errors.New("fallback row used")}
	if err := QueryRow(context.Background(), fb, "select 1").Scan(nil); err == nil || err.Error() != "fallback row used" {
		t.Fatalf("QueryRow.Scan err = %v, want fallback row sentinel", err)
	}
	if fb.rowCalls != 1 {
		t.Errorf("fallback.QueryRow calls = %d, want 1", fb.rowCalls)
	}
}
