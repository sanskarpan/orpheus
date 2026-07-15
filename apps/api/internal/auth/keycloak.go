package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/lestrrat-go/jwx/v2/jwk"
	"github.com/lestrrat-go/jwx/v2/jwt"

	"github.com/orpheus/api/internal/config"
)

// KeycloakVerifier validates Keycloak-issued JWTs against the realm's
// JWKS endpoint. A single verifier is constructed at startup; the
// underlying jwk.Cache transparently refreshes the keyset on key
// rotation.
//
// The verifier assumes the realm has been configured with:
//   - a custom claim `org_id` (string, UUID) on every token; the value
//     drives the RLS tenant scope
//   - realm roles in `realm_access.roles` (standard Keycloak shape)
//
// If the realm is not yet configured to emit `org_id` we fall back to
// [DefaultOrgID] — but ONLY in non-prod. In production a token without
// org_id is rejected: silently mapping every such token to a single
// shared org would let unrelated users read and write each other's data
// (the org_id is the RLS tenant scope). allowDefaultOrg gates this and
// is derived from cfg.IsProd() at construction.
type KeycloakVerifier struct {
	jwks            jwk.Set
	issuer          string
	audience        string
	clock           func() time.Time
	defaultOrg      string
	allowDefaultOrg bool
}

// DefaultOrgID is the org_id used when a token has no `org_id` claim.
// It is a const so the value is easy to find and override in tests.
const DefaultOrgID = "00000000-0000-0000-0000-000000000000"

// NewKeycloakVerifier fetches the realm's JWKS once at startup and
// returns a verifier bound to cfg.KeycloakURL + cfg.KeycloakRealm
// and configured to accept tokens whose `aud` matches
// cfg.KeycloakClientID.
func NewKeycloakVerifier(ctx context.Context, cfg *config.Config) (*KeycloakVerifier, error) {
	jwksURL := strings.TrimRight(cfg.KeycloakURL, "/") +
		"/realms/" + cfg.KeycloakRealm +
		"/protocol/openid-connect/certs"

	cache := jwk.NewCache(ctx)
	if err := cache.Register(jwksURL, jwk.WithMinRefreshInterval(15*time.Minute)); err != nil {
		return nil, fmt.Errorf("auth.keycloak.jwks_register: %w", err)
	}

	// Force the initial fetch so a misconfigured realm URL fails
	// loud at startup, not on the first request.
	set, err := cache.Refresh(ctx, jwksURL)
	if err != nil {
		return nil, fmt.Errorf("auth.keycloak.jwks_fetch: %w", err)
	}

	return newVerifierFromSet(set, cfg)
}

// NewKeycloakVerifierWithSet is the test-only constructor that takes
// an already-resolved jwk.Set. It is exported so the auth_test
// package can wire a hermetic JWKS without standing up Keycloak;
// production code should call [NewKeycloakVerifier].
func NewKeycloakVerifierWithSet(set jwk.Set, cfg *config.Config) (*KeycloakVerifier, error) {
	return newVerifierFromSet(set, cfg)
}

func newVerifierFromSet(set jwk.Set, cfg *config.Config) (*KeycloakVerifier, error) {
	if cfg == nil {
		return nil, errors.New("auth.keycloak: nil config")
	}
	return &KeycloakVerifier{
		jwks:            set,
		issuer:          strings.TrimRight(cfg.KeycloakURL, "/") + "/realms/" + cfg.KeycloakRealm,
		audience:        cfg.KeycloakClientID,
		clock:           time.Now,
		defaultOrg:      DefaultOrgID,
		allowDefaultOrg: !cfg.IsProd(),
	}, nil
}

// Verify validates token and returns the corresponding [Principal].
//
// Verification includes signature, issuer, audience, and expiry
// (with a 60-second leeway for clock skew between Keycloak and the
// API). The custom `org_id` claim is the only non-standard claim read;
// the rest come from the standard OIDC set.
func (v *KeycloakVerifier) Verify(ctx context.Context, token string) (*Principal, error) {
	if token == "" {
		return nil, errors.New("auth.keycloak: empty token")
	}

	parsed, err := jwt.Parse([]byte(token),
		jwt.WithKeySet(v.jwks),
		jwt.WithIssuer(v.issuer),
		jwt.WithAudience(v.audience),
		jwt.WithValidate(true),
		jwt.WithAcceptableSkew(60*time.Second),
	)
	if err != nil {
		return nil, fmt.Errorf("auth.keycloak.verify: %w", err)
	}

	orgID := stringClaim(parsed, "org_id")
	if orgID == "" {
		if !v.allowDefaultOrg {
			return nil, errors.New("auth.keycloak: token missing org_id claim")
		}
		orgID = v.defaultOrg
	}

	sub := stringClaim(parsed, "sub")
	if sub == "" {
		return nil, errors.New("auth.keycloak: sub claim missing")
	}

	return &Principal{
		OrgID:  orgID,
		UserID: sub,
		Email:  stringClaim(parsed, "email"),
		Roles:  realmRoles(parsed),
	}, nil
}

// stringClaim returns a string claim or "" if it is missing or not a
// string. jwt.Token.Get returns interface{}, so we need a small
// helper to avoid scattering type assertions.
func stringClaim(t jwt.Token, name string) string {
	v, ok := t.Get(name)
	if !ok {
		return ""
	}
	s, _ := v.(string)
	return s
}

// realmRoles extracts `realm_access.roles`, the standard Keycloak
// location for realm-level role assignments. Roles drive
// authorization in downstream handlers (e.g. the "service" role
// implies IsService = true).
func realmRoles(t jwt.Token) []string {
	v, ok := t.Get("realm_access")
	if !ok {
		return nil
	}
	m, ok := v.(map[string]interface{})
	if !ok {
		return nil
	}
	raw, ok := m["roles"]
	if !ok {
		return nil
	}
	list, ok := raw.([]interface{})
	if !ok {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, r := range list {
		if s, ok := r.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
