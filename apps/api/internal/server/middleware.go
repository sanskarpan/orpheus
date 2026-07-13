package server

import (
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"

	"github.com/orpheus/api/internal/metrics"
)

func MetricsMiddleware(m *metrics.Metrics) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := chimw.NewWrapResponseWriter(w, r.ProtoMajor)
			next.ServeHTTP(rec, r)
			route := chi.RouteContext(r.Context()).RoutePattern()
			if route == "" {
				route = "unknown"
			}
			status := strconv.Itoa(rec.Status())
			m.HTTPRequests.WithLabelValues(r.Method, route, status).Inc()
			m.HTTPDuration.WithLabelValues(r.Method, route, status).Observe(time.Since(start).Seconds())
		})
	}
}
