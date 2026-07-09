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
