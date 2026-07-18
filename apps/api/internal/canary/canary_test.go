package canary

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestProbeOnce_Healthy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	reg := prometheus.NewRegistry()
	p := New(srv.URL, reg)
	if !p.ProbeOnce(context.Background()) {
		t.Fatal("expected ProbeOnce to succeed against a 200 server")
	}
	if got := testutil.ToFloat64(p.up.WithLabelValues("/health")); got != 1 {
		t.Fatalf("canary_up{/health} = %v, want 1", got)
	}
	if got := testutil.ToFloat64(p.up.WithLabelValues("/ready")); got != 1 {
		t.Fatalf("canary_up{/ready} = %v, want 1", got)
	}
}

func TestProbeOnce_Down(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	reg := prometheus.NewRegistry()
	p := New(srv.URL, reg)
	if p.ProbeOnce(context.Background()) {
		t.Fatal("expected ProbeOnce to fail against a 500 server")
	}
	if got := testutil.ToFloat64(p.up.WithLabelValues("/health")); got != 0 {
		t.Fatalf("canary_up{/health} = %v, want 0", got)
	}
	if got := testutil.ToFloat64(p.total.WithLabelValues("/health", "failure")); got != 1 {
		t.Fatalf("probes_total{failure} = %v, want 1", got)
	}
}

func TestProbe_Unreachable(t *testing.T) {
	reg := prometheus.NewRegistry()
	// Nothing listening on this port.
	p := New("http://127.0.0.1:1", reg)
	if p.ProbeOnce(context.Background()) {
		t.Fatal("expected ProbeOnce to fail against an unreachable target")
	}
	if got := testutil.ToFloat64(p.up.WithLabelValues("/health")); got != 0 {
		t.Fatalf("canary_up{/health} = %v, want 0 when unreachable", got)
	}
}
