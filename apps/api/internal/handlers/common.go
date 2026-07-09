// Package handlers — shared response helpers.
//
// The handlers in this package emit JSON success bodies and RFC 7807
// problem+json error bodies. The helpers below keep the wire format in
// one place so individual handlers stay focused on business logic and
// so the Content-Type / status code combination is consistent across
// the surface. The writeJSON helper lives next to Liveness/Readiness
// in health.go to keep that file self-contained; it is intentionally
// not duplicated here.
package handlers

import (
	"encoding/json"
	"net/http"
)

// writeProblem emits an RFC 7807 problem+json error body. The
// `type` URI is namespaced under https://docs.orpheus.dev/errors/ so
// clients can switch on it without parsing the human-readable
// `title` or `detail`. Encoding errors are intentionally swallowed:
// by the time we are writing the body the status code and headers
// have already been sent, so there is nothing useful to do. A future
// iteration could add structured logging for these failures.
func writeProblem(w http.ResponseWriter, status int, kind, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type":   "https://docs.orpheus.dev/errors/" + kind,
		"title":  http.StatusText(status),
		"status": status,
		"detail": detail,
	})
}

// nullStringVal dereferences a *string, returning "" for nil. It is
// useful when a struct field is a *string (so we can distinguish
// "missing" from "empty string" in the wire format) but a downstream
// helper needs a plain string. Kept as a tiny package-level helper
// rather than inlined at each call site.
func nullStringVal(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
