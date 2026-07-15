package idempotency

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/orpheus/api/internal/auth"
	"github.com/orpheus/api/internal/db"
)

// TestHandler_ReserveReplayConflict is the regression test for the
// reserve-before-execute flow (concurrent double-apply), the
// method+path+body scoping (cross-endpoint replay), and the RLS bug
// (idempotency_keys is FORCE-RLS, so the middleware must run under
// WithTenant or it silently caches nothing).
func TestHandler_ReserveReplayConflict(t *testing.T) {
	dsn := os.Getenv("ORPHEUS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ORPHEUS_TEST_DATABASE_URL not set; skipping live-db idempotency test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pool, err := db.New(ctx, dsn)
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	t.Cleanup(pool.Close)

	svc := servicePool(t, dsn) // is_service on every conn, for seed/cleanup
	orgID := uuid.NewString()
	if _, err := svc.Exec(ctx, `INSERT INTO organizations (id, name, slug) VALUES ($1, $2, $2)`, orgID, "idem-"+orgID); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer ccancel()
		_, _ = svc.Exec(cctx, `DELETE FROM idempotency_keys WHERE org_id=$1`, orgID)
		_, _ = svc.Exec(cctx, `DELETE FROM organizations WHERE id=$1`, orgID)
	})

	var calls int
	h := New(pool).Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"n":`+itoa(calls)+`}`)
	}))

	key := "idem-" + uuid.NewString()
	do := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set(HeaderName, key)
		req = req.WithContext(auth.WithPrincipal(req.Context(), &auth.Principal{OrgID: orgID}))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	// 1) First request: handler runs, 201, cached.
	r1 := do(http.MethodPost, "/v1/uploads", `{"a":1}`)
	if r1.Code != http.StatusCreated || calls != 1 {
		t.Fatalf("first request: code=%d calls=%d, want 201/1 (RLS drain broken?)", r1.Code, calls)
	}

	// 2) Same key+path+body: replayed, handler NOT run again.
	r2 := do(http.MethodPost, "/v1/uploads", `{"a":1}`)
	if r2.Code != http.StatusCreated || calls != 1 {
		t.Fatalf("replay: code=%d calls=%d, want 201/1 (handler re-ran)", r2.Code, calls)
	}
	if r2.Header().Get(replayHeader) != "true" {
		t.Errorf("replay header not set on cached response")
	}
	// Bodies are compared JSON-semantically: response_body is a jsonb
	// column, so Postgres normalises whitespace on the round-trip.
	if !jsonEqual(t, r2.Body.Bytes(), r1.Body.Bytes()) {
		t.Errorf("replay body = %q, want json-equal to %q", r2.Body.String(), r1.Body.String())
	}

	// 3) Same key, different body → 409 conflict, handler NOT run.
	r3 := do(http.MethodPost, "/v1/uploads", `{"a":2}`)
	if r3.Code != http.StatusConflict || calls != 1 {
		t.Fatalf("body conflict: code=%d calls=%d, want 409/1", r3.Code, calls)
	}

	// 4) Same key, same body, DIFFERENT endpoint → 409 (cross-endpoint).
	r4 := do(http.MethodPost, "/v1/jobs", `{"a":1}`)
	if r4.Code != http.StatusConflict || calls != 1 {
		t.Fatalf("cross-endpoint: code=%d calls=%d, want 409/1", r4.Code, calls)
	}
}

func servicePool(t *testing.T, dsn string) *db.DB {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SET app.is_service = 'true'")
		return err
	}
	p, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("NewWithConfig: %v", err)
	}
	t.Cleanup(p.Close)
	return &db.DB{Pool: p}
}

func jsonEqual(t *testing.T, a, b []byte) bool {
	t.Helper()
	var av, bv any
	if err := json.Unmarshal(a, &av); err != nil {
		return false
	}
	if err := json.Unmarshal(b, &bv); err != nil {
		return false
	}
	return reflect.DeepEqual(av, bv)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
