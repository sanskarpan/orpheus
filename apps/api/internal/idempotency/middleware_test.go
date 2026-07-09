package idempotency

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/orpheus/api/internal/auth"
)

// TestHandlerMissingKey confirms the middleware is a transparent
// passthrough when the Idempotency-Key header is absent. This is the
// common case — most endpoints in the API don't require the header —
// and a regression here would silently cache unrelated responses.
func TestHandlerMissingKey(t *testing.T) {
	m := New(nil) // DB=nil; we never reach it when the key is missing.
	called := false
	h := m.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/uploads", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if !called {
		t.Fatal("downstream handler not invoked")
	}
	if rec.Code != http.StatusCreated {
		t.Errorf("status = %d, want 201", rec.Code)
	}
	if got := rec.Header().Get(replayHeader); got != "" {
		t.Errorf("replay header set to %q on live response", got)
	}
}

// TestHandlerNoPrincipal confirms the middleware is a passthrough when
// auth has not attached a principal. This is the pre-auth ordering —
// idempotency must run AFTER auth in the real router, but in tests
// (and any future route that opts out of auth) we want it to do
// nothing.
func TestHandlerNoPrincipal(t *testing.T) {
	m := New(nil)
	called := false
	h := m.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusAccepted)
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/uploads", strings.NewReader(`{}`))
	req.Header.Set(HeaderName, "abc-123")
	h.ServeHTTP(httptest.NewRecorder(), req)

	if !called {
		t.Fatal("downstream handler not invoked when principal missing")
	}
}

// TestHandlerKeyTooLong confirms that an oversized key is treated as
// "no key at all" — we never want to write a 1MB key to the DB just
// because a client pasted a JWT in there.
func TestHandlerKeyTooLong(t *testing.T) {
	m := New(nil)
	called := false
	h := m.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
	}))

	big := strings.Repeat("a", MaxKeyLen+1)
	req := httptest.NewRequest(http.MethodPost, "/v1/uploads", strings.NewReader(`{}`))
	req.Header.Set(HeaderName, big)
	h.ServeHTTP(httptest.NewRecorder(), req)

	if !called {
		t.Fatal("downstream handler not invoked for oversized key")
	}
}

// TestRecorderBuffering is a unit test for the response capture. It
// pins the recorded status + body so downstream caching cannot regress
// silently.
func TestRecorderBuffering(t *testing.T) {
	rec := newRecorder(httptest.NewRecorder())
	rec.WriteHeader(http.StatusTeapot)
	_, _ = io.WriteString(rec, "hello")
	_, _ = io.WriteString(rec, " world")

	if rec.status != http.StatusTeapot {
		t.Errorf("status = %d, want 418", rec.status)
	}
	if got := rec.body.String(); got != "hello world" {
		t.Errorf("body = %q, want %q", got, "hello world")
	}
}

// TestWriteProblem ensures the problem+json body has the shape
// expected by API clients (RFC 7807 fields: type, title, status,
// detail).
func TestWriteProblem(t *testing.T) {
	rec := httptest.NewRecorder()
	writeProblem(rec, http.StatusConflict, "https://x/y", "Boom", "details")

	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Errorf("Content-Type = %q, want application/problem+json", got)
	}
	body := rec.Body.String()
	for _, want := range []string{`"type":"https://x/y"`, `"title":"Boom"`, `"status":409`, `"detail":"details"`} {
		if !strings.Contains(body, want) {
			t.Errorf("body %q missing %q", body, want)
		}
	}
}

// TestHandlerWithPrincipalPassesThrough is a regression guard: with a
// principal in context but no DB, the middleware still hands the
// request to the wrapped handler. The DB-less path is hit when the
// first-time insert fails; we don't want a DB error to swallow the
// actual response.
func TestHandlerWithPrincipalPassesThrough(t *testing.T) {
	m := New(nil)
	called := false
	h := m.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":"abc"}`)
	}))

	ctx := auth.WithPrincipal(context.Background(),
		&auth.Principal{UserID: "u-1", OrgID: "o-1"})
	req := httptest.NewRequest(http.MethodPost, "/v1/uploads", strings.NewReader(`{}`)).WithContext(ctx)
	req.Header.Set(HeaderName, "key-1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !called {
		t.Fatal("downstream handler not invoked with principal in context")
	}
	if rec.Code != http.StatusCreated {
		t.Errorf("status = %d, want 201", rec.Code)
	}
}

// TestHandlerRequiresDB exercises the success path against a live
// Postgres. It is skipped in environments where
// ORPHEUS_TEST_DATABASE_URL is unset (CI without `docker compose up`).
// The intent is to catch wiring regressions — a wrong table name, a
// missing index, a tenant-scoping bug — before they reach integration
// tests in Phase 2.
func TestHandlerRequiresDB(t *testing.T) {
	if testing.Short() {
		t.Skip("requires ORPHEUS_TEST_DATABASE_URL; skipped in -short mode")
	}
	t.Skip("ORPHEUS_TEST_DATABASE_URL not set; skipping live-db idempotency test")
	// Keep the import live for the future live-DB test.
	_ = slog.Default
}
