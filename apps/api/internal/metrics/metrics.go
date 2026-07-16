// Package metrics owns the Prometheus collectors. Pass *Metrics into the
// HTTP middleware, the outbox publisher, the handlers, and the dbtx
// package; they record to the same struct.
//
// Each New() builds collectors against its OWN registry (exposed as
// Registry) rather than the global default. This makes New() safe to
// call more than once in a single process — the previous default-registry
// approach panicked on the second call ("duplicate metrics collector
// registration"), which made the e2e suite impossible to run as a whole.
// Serve /metrics from m.Registry.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

type Metrics struct {
	Registry             *prometheus.Registry
	HTTPRequests         *prometheus.CounterVec
	HTTPDuration         *prometheus.HistogramVec
	JobsSubmitted        *prometheus.CounterVec
	OutboxPublished      *prometheus.CounterVec
	OutboxPublishLatency *prometheus.HistogramVec
	RLSDenials           *prometheus.CounterVec
}

func New() *Metrics {
	reg := prometheus.NewRegistry()
	// Keep the standard Go runtime / process metrics that the default
	// registry would have provided.
	reg.MustRegister(collectors.NewGoCollector())
	reg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	f := promauto.With(reg)
	return &Metrics{
		Registry: reg,
		HTTPRequests: f.NewCounterVec(
			prometheus.CounterOpts{
				Name: "orpheus_http_requests_total",
				Help: "Total HTTP requests, labeled by method, route, and status code.",
			},
			[]string{"method", "route", "status"},
		),
		HTTPDuration: f.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "orpheus_http_request_duration_seconds",
				Help:    "HTTP request duration in seconds, labeled by method, route, and status code.",
				Buckets: prometheus.DefBuckets,
			},
			[]string{"method", "route", "status"},
		),
		JobsSubmitted: f.NewCounterVec(
			prometheus.CounterOpts{
				Name: "orpheus_jobs_submitted_total",
				Help: "Total jobs submitted via POST /v1/jobs, labeled by processor name.",
			},
			[]string{"processor"},
		),
		OutboxPublished: f.NewCounterVec(
			prometheus.CounterOpts{
				Name: "orpheus_outbox_published_total",
				Help: "Total outbox events published to NATS, labeled by event_type and result (success/error).",
			},
			[]string{"event_type", "result"},
		),
		OutboxPublishLatency: f.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "orpheus_outbox_publish_duration_seconds",
				Help:    "Outbox publish latency in seconds, labeled by event_type.",
				Buckets: prometheus.DefBuckets,
			},
			[]string{"event_type"},
		),
		RLSDenials: f.NewCounterVec(
			prometheus.CounterOpts{
				Name: "orpheus_rls_denials_total",
				Help: "Total row-level-security denials, labeled by table.",
			},
			[]string{"table"},
		),
	}
}
