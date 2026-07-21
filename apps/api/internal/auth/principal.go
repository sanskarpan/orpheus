// Package auth contains the authentication layer for the Orpheus API.
//
// Every /v1/... request is wrapped in [Authenticator.Middleware], which
// resolves a [Principal] from either a Keycloak bearer token (OIDC/JWKS)
// or an X-API-Key header, then attaches the principal to the request
// context. Downstream code retrieves the principal via
// [PrincipalFromContext] and uses it (chiefly OrgID) to scope every
// database query through the row-level-security tenant helper.
package auth

import (
	"context"
	"errors"
	"net/http"
)

// Principal is the authenticated identity attached to a request.
//
// Exactly one of UserID and APIKeyID is set: UserID for JWT auth
// (interactive user, populated from the OIDC `sub` claim) and APIKeyID
// for API-key auth (machine actor). The OrgID is always set; it is the
// RLS scope used by every database query in the request.
//
// Roles carry authorization context. For JWTs they come from the
// Keycloak `realm_access.roles` claim; for API keys they come from the
// `scopes` column on the api_keys row, which doubles as the API key's
// "role set" for the same authorization checks.
type Principal struct {
	OrgID     string
	UserID    string // empty for API key auth
	APIKeyID  string // empty for JWT auth
	Email     string // from JWT claims; empty for API key auth
	Roles     []string
	IsService bool // true for the service role (bypass RLS)
	// OrgSuspended is consumed by the Rego authorization policy (a suspended
	// org may read but not mutate). It is a hook: the auth layer does not yet
	// populate it from org status, so it defaults false and the deny-override
	// is inert until that lookup is wired.
	OrgSuspended bool
}

// validScopes is the set of API-key scope strings the API recognises,
// mirroring the APIKeyScope enum in the OpenAPI spec. "*" means full
// org access.
var validScopes = map[string]struct{}{
	"uploads:read": {}, "uploads:write": {},
	"artifacts:read": {}, "artifacts:write": {},
	"jobs:read": {}, "jobs:write": {},
	"webhooks:read": {}, "webhooks:write": {},
	"usage:read": {}, "audit:read": {},
	"pii:unmask":        {}, // fetch a pii_mapping-sensitivity artifact (PRD 08)
	"data:erase":        {}, // tenant-initiated GDPR erasure (PRD 10)
	"streaming:read":    {}, // inspect/list streaming sessions (Phase 8)
	"streaming:write":   {}, // create/finalize streaming sessions (Phase 8)
	"marketplace:read":  {}, // browse the processor marketplace (Phase 7)
	"marketplace:write": {}, // submit a community processor (Phase 7)
	"*":                 {},
}

// IsValidScope reports whether s is a recognised API-key scope.
func IsValidScope(s string) bool {
	_, ok := validScopes[s]
	return ok
}

// CanGrantScope reports whether this principal is allowed to grant
// `scope` to a new API key. An interactive user (JWT) has full authority
// within its org. An API-key principal may only grant scopes it already
// holds (or anything if it holds the "*" wildcard) — this prevents a
// narrow key from minting a broader one (privilege escalation).
func (p *Principal) CanGrantScope(scope string) bool {
	if p == nil {
		return false
	}
	if p.APIKeyID == "" {
		return true // JWT / interactive user
	}
	for _, held := range p.Roles {
		if held == "*" || held == scope {
			return true
		}
	}
	return false
}

// HasScope reports whether the principal is authorised for `scope`. An
// interactive user (JWT) has full org authority. An API-key principal is
// authorised only if its scope set contains `scope` or the "*" wildcard.
func (p *Principal) HasScope(scope string) bool {
	if p == nil {
		return false
	}
	if p.APIKeyID == "" {
		return true // JWT / interactive user
	}
	for _, held := range p.Roles {
		if held == "*" || held == scope {
			return true
		}
	}
	return false
}

// RequireScope returns middleware that rejects (403) an authenticated
// API-key principal lacking `scope`. It must be mounted under the auth
// middleware. JWT principals pass through (full org authority). A missing
// principal is treated as forbidden — the auth middleware should already
// have rejected it upstream, so this is defence in depth.
func RequireScope(scope string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, err := PrincipalFromContext(r.Context())
			if err != nil || !p.HasScope(scope) {
				w.Header().Set("Content-Type", "application/problem+json")
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"type":"https://docs.orpheus.dev/errors/forbidden","title":"Forbidden","status":403,"detail":"missing required scope: ` + scope + `"}`))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// ctxKey is the unexported context key used for principal lookups.
// Unexported types guarantee no other package can collide with the key.
type ctxKey struct{}

// WithPrincipal returns a new context with p attached.
func WithPrincipal(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

// PrincipalFromContext returns the principal stored on ctx, or an
// error if no principal is present. Handlers behind the auth
// middleware should call this once and propagate the error.
func PrincipalFromContext(ctx context.Context) (*Principal, error) {
	p, ok := ctx.Value(ctxKey{}).(*Principal)
	if !ok || p == nil {
		return nil, errors.New("auth: no principal in context")
	}
	return p, nil
}

// MustPrincipal returns the principal or panics. It is intended for
// handlers that are mounted *under* the auth middleware and can
// therefore guarantee a principal is present. Calling it from a
// handler that is not behind the middleware will panic; that is
// deliberate — a missing principal in such a handler is a wiring bug
// that should surface loudly during development, not silently degrade
// to no-auth at runtime.
func MustPrincipal(ctx context.Context) *Principal {
	p, err := PrincipalFromContext(ctx)
	if err != nil {
		panic(err)
	}
	return p
}
