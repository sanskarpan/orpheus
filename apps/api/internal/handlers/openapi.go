package handlers

import (
	_ "embed"
	"net/http"
)

//go:embed openapi.json
var openAPISpec []byte

// OpenAPISpec serves the embedded OpenAPI document with the correct
// content type for consumption by Swagger UI, ReDoc, code generators, etc.
//
// The source file lives at internal/handlers/openapi.json. A duplicate is
// kept at api/openapi.json for tooling that expects the spec at the project
// root (e.g. hand-rolled docs, contract tests); keep them in sync.
func OpenAPISpec(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(openAPISpec)
}
