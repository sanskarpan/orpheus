package handlers

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics serves the Prometheus exposition format from the default registry.
//
// The default registry is auto-populated with Go runtime metrics and process
// metrics by the [prometheus] package's init function, so no setup is
// required in Phase 0. Phase 1+ will register domain-specific counters and
// histograms (request durations, jobs enqueued, etc.).
//
// We wrap [promhttp.HandlerFor] rather than calling [promhttp.Handler] so
// the Gatherer is explicit and easy to swap out in tests.
func Metrics(w http.ResponseWriter, r *http.Request) {
	h := promhttp.HandlerFor(prometheus.DefaultGatherer, promhttp.HandlerOpts{})
	h.ServeHTTP(w, r)
}
