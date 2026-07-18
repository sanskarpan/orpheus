// Package canary is a synthetic prober: it periodically checks the Orpheus
// API's user-facing liveness/readiness endpoints from the outside and records
// the outcome as Prometheus metrics. An external canary catches "the API is
// unreachable to clients" even when internal metrics look healthy (e.g. the
// ingress/LB is broken), which is exactly the blind spot self-scraped metrics
// have. Prometheus alerts on orpheus_canary_up == 0 or a stale scrape.
package canary

import (
	"context"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Prober probes a set of endpoints on the target API base URL.
type Prober struct {
	BaseURL   string
	Client    *http.Client
	Endpoints []string

	up      *prometheus.GaugeVec
	total   *prometheus.CounterVec
	latency *prometheus.HistogramVec
}

// New builds a Prober and registers its metrics with reg. It probes /health
// and /ready by default.
func New(baseURL string, reg prometheus.Registerer) *Prober {
	up := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "orpheus_canary_up",
		Help: "1 if the last synthetic probe of the endpoint succeeded, else 0.",
	}, []string{"endpoint"})
	total := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "orpheus_canary_probes_total",
		Help: "Total synthetic canary probes by endpoint and result (success/failure).",
	}, []string{"endpoint", "result"})
	latency := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "orpheus_canary_probe_duration_seconds",
		Help:    "Synthetic canary probe latency by endpoint.",
		Buckets: prometheus.DefBuckets,
	}, []string{"endpoint"})
	reg.MustRegister(up, total, latency)
	return &Prober{
		BaseURL:   baseURL,
		Client:    &http.Client{Timeout: 10 * time.Second},
		Endpoints: []string{"/health", "/ready"},
		up:        up,
		total:     total,
		latency:   latency,
	}
}

// ProbeOnce probes every endpoint once and returns true iff all succeeded.
func (p *Prober) ProbeOnce(ctx context.Context) bool {
	allOK := true
	for _, ep := range p.Endpoints {
		if !p.probe(ctx, ep) {
			allOK = false
		}
	}
	return allOK
}

func (p *Prober) probe(ctx context.Context, endpoint string) bool {
	start := time.Now()
	ok := false
	if req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.BaseURL+endpoint, nil); err == nil {
		if resp, err := p.Client.Do(req); err == nil {
			_ = resp.Body.Close()
			ok = resp.StatusCode >= 200 && resp.StatusCode < 300
		}
	}
	p.latency.WithLabelValues(endpoint).Observe(time.Since(start).Seconds())
	result := "failure"
	if ok {
		p.up.WithLabelValues(endpoint).Set(1)
		result = "success"
	} else {
		p.up.WithLabelValues(endpoint).Set(0)
	}
	p.total.WithLabelValues(endpoint, result).Inc()
	return ok
}

// Run probes immediately and then every interval until ctx is cancelled.
func (p *Prober) Run(ctx context.Context, interval time.Duration) {
	p.ProbeOnce(ctx)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.ProbeOnce(ctx)
		}
	}
}
