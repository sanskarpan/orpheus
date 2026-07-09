package auth_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alexedwards/argon2id"
	"github.com/lestrrat-go/jwx/v2/jwa"
	"github.com/lestrrat-go/jwx/v2/jwk"
	"github.com/lestrrat-go/jwx/v2/jwt"

	"github.com/orpheus/api/internal/auth"
	"github.com/orpheus/api/internal/config"
)

// ─────────────────────────────────────────────────────────────────────
// Principal context helpers
// ─────────────────────────────────────────────────────────────────────

func TestPrincipalContext_RoundTrip(t *testing.T) {
	p := &auth.Principal{OrgID: "test-org", UserID: "test-user"}
	ctx := auth.WithPrincipal(context.Background(), p)

	got, err := auth.PrincipalFromContext(ctx)
	if err != nil {
		t.Fatalf("PrincipalFromContext: %v", err)
	}
	if got.OrgID != "test-org" {
		t.Errorf("OrgID = %q, want test-org", got.OrgID)
	}
	if got.UserID != "test-user" {
		t.Errorf("UserID = %q, want test-user", got.UserID)
	}
}

func TestPrincipalContext_Missing(t *testing.T) {
	_, err := auth.PrincipalFromContext(context.Background())
	if err == nil {
		t.Error("expected error for missing principal")
	}
}

func TestMustPrincipal_PanicsOnMissing(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic on missing principal")
		}
	}()
	_ = auth.MustPrincipal(context.Background())
}

// ─────────────────────────────────────────────────────────────────────
// Authenticator dispatch
// ─────────────────────────────────────────────────────────────────────

func TestAuthenticate_NoCredentials(t *testing.T) {
	a := &auth.Authenticator{}
	req := httptest.NewRequest("GET", "/test", nil)
	rec := httptest.NewRecorder()
	a.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("handler should not be called when auth fails")
	})).ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/problem+json") {
		t.Errorf("Content-Type = %q, want application/problem+json", got)
	}
}

func TestAuthenticate_BearerButKeycloakUnconfigured(t *testing.T) {
	a := &auth.Authenticator{} // Keycloak is nil
	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("Authorization", "Bearer some-token")

	rec := httptest.NewRecorder()
	a.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).
		ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "bearer auth not configured") {
		t.Errorf("body should explain misconfiguration, got: %s", rec.Body.String())
	}
}

func TestAuthenticate_APIKeyButValidatorUnconfigured(t *testing.T) {
	a := &auth.Authenticator{} // APIKey is nil
	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("X-API-Key", "ak_live_abcdef")

	rec := httptest.NewRecorder()
	a.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).
		ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "api key auth not configured") {
		t.Errorf("body should explain misconfiguration, got: %s", rec.Body.String())
	}
}

func TestAuthenticate_EmptyBearerToken(t *testing.T) {
	a := &auth.Authenticator{
		Keycloak: &auth.KeycloakVerifier{}, // not nil, but token is empty so Verify rejects
	}
	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("Authorization", "Bearer ")

	rec := httptest.NewRecorder()
	a.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).
		ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestMiddleware_AttachesPrincipalToContext(t *testing.T) {
	// Bypass real verification: stub a verifier by using an API key
	// path with a stub lookup. This exercises the full middleware
	// plumbing (auth → principal → context → handler) without
	// standing up Keycloak.
	hash, err := argon2id.CreateHash("ak_live_secret123", argon2id.DefaultParams)
	if err != nil {
		t.Fatalf("argon2id.CreateHash: %v", err)
	}

	lookup := &stubLookup{
		records: map[string]auth.APIKeyRecord{
			"ak_live_s": {
				ID:           "key-1",
				OrgID:        "org-1",
				HashedSecret: hash,
				Prefix:       "ak_live_s",
				Scopes:       []string{"read", "write"},
			},
		},
	}
	a := &auth.Authenticator{APIKey: auth.NewAPIKeyValidator(lookup)}

	req := httptest.NewRequest("GET", "/v1/things", nil)
	req.Header.Set("X-API-Key", "ak_live_secret123")

	var seen *auth.Principal
	rec := httptest.NewRecorder()
	a.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		p, err := auth.PrincipalFromContext(r.Context())
		if err != nil {
			t.Errorf("PrincipalFromContext: %v", err)
			return
		}
		seen = p
	})).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if seen == nil {
		t.Fatal("handler did not see a principal")
	}
	if seen.OrgID != "org-1" {
		t.Errorf("OrgID = %q, want org-1", seen.OrgID)
	}
	if seen.APIKeyID != "key-1" {
		t.Errorf("APIKeyID = %q, want key-1", seen.APIKeyID)
	}
}

func TestMiddleware_ProblemJSONIsWellFormed(t *testing.T) {
	a := &auth.Authenticator{}
	req := httptest.NewRequest("GET", "/test", nil)

	rec := httptest.NewRecorder()
	a.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).
		ServeHTTP(rec, req)

	var body struct {
		Type   string `json:"type"`
		Title  string `json:"title"`
		Status int    `json:"status"`
		Detail string `json:"detail"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("response body is not valid JSON: %v — body=%q", err, rec.Body.String())
	}
	if body.Type == "" || body.Status != 401 {
		t.Errorf("problem body malformed: %+v", body)
	}
}

// ─────────────────────────────────────────────────────────────────────
// APIKeyValidator
// ─────────────────────────────────────────────────────────────────────

func TestAPIKeyValidator_RejectsBadFormat(t *testing.T) {
	v := auth.NewAPIKeyValidator(&stubLookup{})
	cases := []string{
		"",
		"ak_",
		"ak_short",               // too short
		"prefix_ak_live_abcdefg", // wrong prefix
		"ak_live_",               // exactly 8 chars, missing secret
	}
	for _, c := range cases {
		if _, err := v.Verify(context.Background(), c); err == nil {
			t.Errorf("Verify(%q) = nil error, want error", c)
		}
	}
}

func TestAPIKeyValidator_NotFound(t *testing.T) {
	v := auth.NewAPIKeyValidator(&stubLookup{notFound: true})
	_, err := v.Verify(context.Background(), "ak_live_unknown")
	if err == nil {
		t.Error("expected error for missing key")
	}
}

func TestAPIKeyValidator_Revoked(t *testing.T) {
	revoked := "2024-01-01T00:00:00Z"
	v := auth.NewAPIKeyValidator(&stubLookup{
		records: map[string]auth.APIKeyRecord{
			"ak_live_x": {
				ID: "k", OrgID: "o", HashedSecret: "x", Prefix: "ak_live_x",
				RevokedAt: &revoked,
			},
		},
	})
	_, err := v.Verify(context.Background(), "ak_live_xany")
	if err == nil || !strings.Contains(err.Error(), "revoked") {
		t.Errorf("err = %v, want revoked", err)
	}
}

func TestAPIKeyValidator_BadHash(t *testing.T) {
	hash, _ := argon2id.CreateHash("ak_live_correct_secret", argon2id.DefaultParams)
	v := auth.NewAPIKeyValidator(&stubLookup{
		records: map[string]auth.APIKeyRecord{
			"ak_live_x": {
				ID: "k", OrgID: "o", HashedSecret: hash, Prefix: "ak_live_x",
			},
		},
	})
	_, err := v.Verify(context.Background(), "ak_live_wrong_secret")
	if err == nil {
		t.Error("expected error for wrong secret")
	}
}

// ─────────────────────────────────────────────────────────────────────
// KeycloakVerifier — uses a self-signed RSA key + in-memory JWKS so
// the test is hermetic (no Keycloak needed). We exercise the
// org_id-from-claim and org_id-default paths.
// ─────────────────────────────────────────────────────────────────────

func newTestKeycloakVerifier(t *testing.T) (*auth.KeycloakVerifier, jwk.Key) {
	t.Helper()

	privKey, err := jwk.FromRaw(rsaTestKey(t))
	if err != nil {
		t.Fatalf("jwk.FromRaw: %v", err)
	}
	if err := privKey.Set(jwk.KeyIDKey, "test-kid"); err != nil {
		t.Fatalf("set kid: %v", err)
	}
	if err := privKey.Set(jwk.AlgorithmKey, jwa.RS256); err != nil {
		t.Fatalf("set alg: %v", err)
	}

	pubKey, err := privKey.PublicKey()
	if err != nil {
		t.Fatalf("PublicKey: %v", err)
	}
	set := jwk.NewSet()
	if err := set.AddKey(pubKey); err != nil {
		t.Fatalf("set.AddKey: %v", err)
	}

	v, err := auth.NewKeycloakVerifierWithSet(set, &config.Config{
		KeycloakURL:      "https://kc.test",
		KeycloakRealm:    "orpheus",
		KeycloakClientID: "orpheus-api",
	})
	if err != nil {
		t.Fatalf("NewKeycloakVerifierWithSet: %v", err)
	}
	return v, privKey
}

func mintToken(t *testing.T, key jwk.Key, claims map[string]any) string {
	t.Helper()
	tok, err := jwt.NewBuilder().
		Issuer("https://kc.test/realms/orpheus").
		Audience([]string{"orpheus-api"}).
		Subject("user-uuid-1").
		IssuedAt(time.Now()).
		Expiration(time.Now().Add(5 * time.Minute)).
		JwtID("jti-1").
		Build()
	if err != nil {
		t.Fatalf("jwt.NewBuilder.Build: %v", err)
	}
	for k, v := range claims {
		if err := tok.Set(k, v); err != nil {
			t.Fatalf("tok.Set %s: %v", k, err)
		}
	}
	signed, err := jwt.Sign(tok, jwt.WithKey(jwa.RS256, key))
	if err != nil {
		t.Fatalf("jwt.Sign: %v", err)
	}
	return string(signed)
}

func TestKeycloakVerifier_HappyPath(t *testing.T) {
	v, key := newTestKeycloakVerifier(t)
	tok := mintToken(t, key, map[string]any{
		"email":  "alice@example.com",
		"org_id": "00000000-0000-0000-0000-000000000abc",
		"realm_access": map[string]any{
			"roles": []any{"user", "viewer"},
		},
	})

	p, err := v.Verify(context.Background(), tok)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if p.OrgID != "00000000-0000-0000-0000-000000000abc" {
		t.Errorf("OrgID = %q", p.OrgID)
	}
	if p.UserID != "user-uuid-1" {
		t.Errorf("UserID = %q", p.UserID)
	}
	if p.Email != "alice@example.com" {
		t.Errorf("Email = %q", p.Email)
	}
	if len(p.Roles) != 2 || p.Roles[0] != "user" || p.Roles[1] != "viewer" {
		t.Errorf("Roles = %v", p.Roles)
	}
	if p.APIKeyID != "" {
		t.Errorf("APIKeyID = %q, want empty for JWT auth", p.APIKeyID)
	}
}

func TestKeycloakVerifier_OrgIDFallsBackToDefault(t *testing.T) {
	v, key := newTestKeycloakVerifier(t)
	tok := mintToken(t, key, nil) // no org_id claim
	p, err := v.Verify(context.Background(), tok)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if p.OrgID != auth.DefaultOrgID {
		t.Errorf("OrgID = %q, want default %q", p.OrgID, auth.DefaultOrgID)
	}
}

func TestKeycloakVerifier_RejectsBadSignature(t *testing.T) {
	v, key := newTestKeycloakVerifier(t)
	tok := mintToken(t, key, nil)

	// Flip a character in the signature.
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("malformed jwt: %q", tok)
	}
	bad := parts[0] + "." + parts[1] + "." + strings.ReplaceAll(parts[2], "A", "B")
	if bad == tok {
		// Force a difference.
		bad = parts[0] + "." + parts[1] + "." + parts[2] + "x"
	}

	if _, err := v.Verify(context.Background(), bad); err == nil {
		t.Error("expected error for tampered token")
	}
}

func TestKeycloakVerifier_RejectsExpiredToken(t *testing.T) {
	v, key := newTestKeycloakVerifier(t)
	tok, err := jwt.NewBuilder().
		Issuer("https://kc.test/realms/orpheus").
		Audience([]string{"orpheus-api"}).
		Subject("user-uuid-1").
		IssuedAt(time.Now().Add(-2 * time.Hour)).
		Expiration(time.Now().Add(-1 * time.Hour)).
		Build()
	if err != nil {
		t.Fatalf("jwt.NewBuilder.Build: %v", err)
	}
	signed, err := jwt.Sign(tok, jwt.WithKey(jwa.RS256, key))
	if err != nil {
		t.Fatalf("jwt.Sign: %v", err)
	}
	if _, err := v.Verify(context.Background(), string(signed)); err == nil {
		t.Error("expected error for expired token")
	}
}

func TestKeycloakVerifier_RejectsWrongAudience(t *testing.T) {
	v, key := newTestKeycloakVerifier(t)
	tok, err := jwt.NewBuilder().
		Issuer("https://kc.test/realms/orpheus").
		Audience([]string{"some-other-client"}).
		Subject("user-uuid-1").
		IssuedAt(time.Now()).
		Expiration(time.Now().Add(5 * time.Minute)).
		Build()
	if err != nil {
		t.Fatalf("jwt.NewBuilder.Build: %v", err)
	}
	signed, err := jwt.Sign(tok, jwt.WithKey(jwa.RS256, key))
	if err != nil {
		t.Fatalf("jwt.Sign: %v", err)
	}
	if _, err := v.Verify(context.Background(), string(signed)); err == nil {
		t.Error("expected error for wrong audience")
	}
}

func TestKeycloakVerifier_RejectsEmptyToken(t *testing.T) {
	v, _ := newTestKeycloakVerifier(t)
	_, err := v.Verify(context.Background(), "")
	if err == nil {
		t.Error("expected error for empty token")
	}
}

// ─────────────────────────────────────────────────────────────────────
// Stubs
// ─────────────────────────────────────────────────────────────────────

type stubLookup struct {
	records  map[string]auth.APIKeyRecord
	notFound bool
}

func (s *stubLookup) GetAPIKeyByPrefix(_ context.Context, prefix string) (auth.APIKeyRecord, error) {
	if s.notFound {
		return auth.APIKeyRecord{}, errors.New("not found")
	}
	r, ok := s.records[prefix]
	if !ok {
		return auth.APIKeyRecord{}, errors.New("not found")
	}
	return r, nil
}
