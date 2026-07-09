package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/orpheus/api/internal/config"
)

// newTestServer builds a Server with a no-op logger so tests do not spam
// stdout. The mux is constructed but not bound to a port — tests drive it
// via httptest.NewRecorder to avoid port allocation and races.
func newTestServer(t *testing.T) *Server {
	t.Helper()
	cfg := &config.Config{
		Env:                  "test",
		Host:                 "127.0.0.1",
		Port:                 0,
		ShutdownGraceSeconds: 1,
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(cfg, logger)
}

func TestLivenessRoute(t *testing.T) {
	s := newTestServer(t)

	rec := call(s, http.MethodGet, "/health", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", got)
	}

	var body map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["status"] != "ok" {
		t.Errorf("body[status] = %q, want ok", body["status"])
	}
}

func TestReadinessRoute(t *testing.T) {
	s := newTestServer(t)

	rec := call(s, http.MethodGet, "/ready", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", got)
	}

	var body struct {
		Status string            `json:"status"`
		Checks map[string]string `json:"checks"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Status != "ready" {
		t.Errorf("body.status = %q, want ready", body.Status)
	}
	if body.Checks["service"] != "ok" {
		t.Errorf("body.checks[service] = %q, want ok", body.Checks["service"])
	}
}

func TestMetricsRoute(t *testing.T) {
	s := newTestServer(t)

	rec := call(s, http.MethodGet, "/metrics", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain", got)
	}
	if !strings.Contains(rec.Body.String(), "# HELP") {
		t.Errorf("metrics body does not contain # HELP — body=%q", firstLine(rec.Body.String()))
	}
}

func TestOpenAPIRoute(t *testing.T) {
	s := newTestServer(t)

	rec := call(s, http.MethodGet, "/api/openapi.json", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", got)
	}

	var spec struct {
		Info struct {
			Title string `json:"title"`
		} `json:"info"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&spec); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if spec.Info.Title != "Orpheus API" {
		t.Errorf("info.title = %q, want Orpheus API", spec.Info.Title)
	}
}

func TestDocsRoute(t *testing.T) {
	s := newTestServer(t)

	rec := call(s, http.MethodGet, "/api/docs", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", got)
	}
	if !strings.Contains(rec.Body.String(), "<!DOCTYPE html>") {
		t.Errorf("body is not HTML")
	}
}

func TestRedocRoute(t *testing.T) {
	s := newTestServer(t)

	rec := call(s, http.MethodGet, "/api/redoc", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", got)
	}
	if !strings.Contains(rec.Body.String(), "redoc") {
		t.Errorf("body does not reference redoc")
	}
}

func TestUnknownRoute(t *testing.T) {
	s := newTestServer(t)

	rec := call(s, http.MethodGet, "/does-not-exist", nil)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func call(s *Server, method, target string, body io.Reader) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, body)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	return rec
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
