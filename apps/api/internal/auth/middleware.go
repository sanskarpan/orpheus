package auth

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

const (
	bearerPrefix = "Bearer "
	apiKeyHeader = "X-API-Key"
)

// problemTypeUnauthenticated is the docs URL emitted in 401
// responses. It is a stable identifier — clients should switch on
// the `type` field, not the human-readable title or detail.
const problemTypeUnauthenticated = "https://docs.orpheus.dev/errors/unauthenticated"

// Authenticator bundles the two verifiers and exposes a single chi
// middleware. Exactly one of Keycloak and APIKey may be nil at
// runtime if that auth method is disabled in a given environment;
// the authenticate method reports a clear error if a request carries
// credentials for a disabled method.
type Authenticator struct {
	Keycloak *KeycloakVerifier
	APIKey   *APIKeyValidator
}

// Middleware returns a chi-compatible http.Handler middleware that
// authenticates every request and attaches the resulting Principal
// to the request context.
//
// Requests with no credentials, or with credentials for a method
// that is not configured, receive a 401 response with an
// application/problem+json body.
func (a *Authenticator) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, err := a.authenticate(r)
		if err != nil {
			writeProblem(w, http.StatusUnauthorized, problemTypeUnauthenticated, "Unauthorized", err.Error())
			return
		}
		next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
	})
}

// authenticate inspects the request headers and dispatches to the
// appropriate verifier. Bearer tokens take precedence over API keys
// when both are present — the Authorization header is the more
// specific signal of caller identity.
func (a *Authenticator) authenticate(r *http.Request) (*Principal, error) {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, bearerPrefix) {
		token := strings.TrimSpace(strings.TrimPrefix(h, bearerPrefix))
		if token == "" {
			return nil, errors.New("auth: empty bearer token")
		}
		if a.Keycloak == nil {
			return nil, errors.New("auth: bearer auth not configured")
		}
		return a.Keycloak.Verify(r.Context(), token)
	}

	if k := r.Header.Get(apiKeyHeader); k != "" {
		if a.APIKey == nil {
			return nil, errors.New("auth: api key auth not configured")
		}
		return a.APIKey.Verify(r.Context(), k)
	}

	return nil, errors.New("auth: no credentials provided")
}

// problemResponse is the RFC 7807 problem+json shape used for error
// responses emitted by middleware. We keep it as a private struct so
// the wire format stays under our control.
type problemResponse struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail"`
}

// writeProblem serializes a problem+json body. The detail string is
// JSON-escaped by json.Marshal, so it is safe to pass arbitrary
// error messages (including ones containing quotes or newlines).
func writeProblem(w http.ResponseWriter, status int, typ, title, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(problemResponse{
		Type:   typ,
		Title:  title,
		Status: status,
		Detail: detail,
	})
}
