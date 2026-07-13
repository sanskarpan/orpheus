// Package metrics owns the Prometheus collectors registered with
// the default registry. Pass *Metrics into the HTTP middleware,
// the outbox publisher, the handlers, and the dbtx package; they
// record to the same struct.
//
// New() uses promauto, which registers with the default registry.
// Calling New() a second time panics on duplicate registration;
// for tests that need a fresh registry, build the collectors
// against prometheus.NewRegistry() directly.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

type Metrics struct {
	HTTPRequests         *prometheus.CounterVec
	HTTPDuration         *prometheus.HistogramVec
	JobsSubmitted        *prometheus.CounterVec
	OutboxPublished      *prometheus.CounterVec
	OutboxPublishLatency *prometheus.HistogramVec
	RLSDenials           *prometheus.CounterVec
}

func New() *Metrics {
	return &Metrics{
		HTTPRequests: promauto.NewCounterVec(
			prometheus.CounterOpts{
				Name: "orpheus_http_requests_total",
				Help: "Total HTTP requests, labeled by method, route, and status code.",
			},
			[]string{"method", "route", "status"},
		),
		HTTPDuration: promauto.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "orpheus_http_request_duration_seconds",
				Help:    "HTTP request duration in seconds, labeled by method, route, and status code.",
				Buckets: prometheus.DefBuckets,
			},
			[]string{"method", "route", "status"},
		),
		JobsSubmitted: promauto.NewCounterVec(
			prometheus.CounterOpts{
				Name: "orpheus_jobs_submitted_total",
				Help: "Total jobs submitted via POST /v1/jobs, labeled by processor name.",
			},
			[]string{"processor"},
		),
		OutboxPublished: promauto.NewCounterVec(
			prometheus.CounterOpts{
				Name: "orpheus_outbox_published_total",
				Help: "Total outbox events published to NATS, labeled by event_type and result (success/error).",
			},
			[]string{"event_type", "result"},
		),
		OutboxPublishLatency: promauto.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "orpheus_outbox_publish_duration_seconds",
				Help:    "Outbox publish latency in seconds, labeled by event_type.",
				Buckets: prometheus.DefBuckets,
			},
			[]string{"event_type"},
		),
		RLSDenials: promauto.NewCounterVec(
			prometheus.CounterOpts{
				Name: "orpheus_rls_denials_total",
				Help: "Total row-level-security denials, labeled by table.",
			},
			[]string{"table"},
		),
	}
}
