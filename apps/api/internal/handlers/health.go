// Package handlers contains the HTTP handlers for the Orpheus API.
//
// Each handler is a plain [net/http] handler func. The server wires them
// into a chi router in [github.com/orpheus/api/internal/server].
package handlers

import (
	"encoding/json"
	"net/http"
)

// livenessResponse is the JSON body returned by [Liveness].
type livenessResponse struct {
	Status string `json:"status"`
}

// readinessResponse is the JSON body returned by [Readiness].
type readinessResponse struct {
	Status string            `json:"status"`
	Checks map[string]string `json:"checks"`
}

// Liveness reports whether the process is alive.
//
// This is a stub in Phase 0; it always returns 200 OK. Container orchestrators
// (Kubernetes, ECS) use this endpoint to decide whether to restart the
// process. It must NOT depend on external services.
func Liveness(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, livenessResponse{Status: "ok"})
}

// Readiness reports whether the service is ready to accept traffic.
//
// This is a stub in Phase 0; it always returns 200 OK with a single
// "service" check. Phase 1+ will fan out to Postgres, Redis, S3, etc.
func Readiness(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, readinessResponse{
		Status: "ready",
		Checks: map[string]string{"service": "ok"},
	})
}

// writeJSON serialises v as JSON with the given status and content type.
// Encoding errors are intentionally swallowed: by the time we are writing
// the body the status code and headers have already been sent, so there is
// nothing useful we can do. A future iteration could add structured logging
// for these failures.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
