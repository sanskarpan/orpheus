package server

import (
	"crypto/rand"
	"crypto/rsa"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lestrrat-go/jwx/v2/jwa"
	"github.com/lestrrat-go/jwx/v2/jwk"
	"github.com/lestrrat-go/jwx/v2/jwt"

	"github.com/orpheus/api/internal/auth"
	"github.com/orpheus/api/internal/config"
)

// newV1Server builds a Server with the v1 surface mounted via an
// authenticator whose verifiers are configured by the caller. The mux
// is not bound to a port; tests drive it through s.mux.ServeHTTP.
func newV1Server(t *testing.T, authn *auth.Authenticator) *Server {
	t.Helper()
	cfg := &config.Config{
		Env:                  "test",
		Host:                 "127.0.0.1",
		Port:                 0,
		ShutdownGraceSeconds: 1,
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewWithOptions(cfg, logger, Options{Authn: authn})
}

// callV1 is a tiny wrapper that drives a request through the server's
// mux without going through the public helper so this file stays
// self-contained.
func callV1(s *Server, method, target string, headers map[string]string) *httptest.ResponseRecorder {
	t := httptest.NewRequest(method, target, nil)
	for k, v := range headers {
		t.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, t)
	return rec
}

// TestV1Route_BearerWithUnconfiguredKeycloak dispatches the bearer
// path of the auth middleware to a Keycloak verifier that is nil.
// The middleware must respond 401 with a problem+json body that
// names the misconfiguration. This pins the dispatch wiring: a
// request carrying `Authorization: Bearer …` reaches the bearer
// branch, not the API-key branch, and the response uses the
// problem+json content type the contract documents.
func TestV1Route_BearerWithUnconfiguredKeycloak(t *testing.T) {
	s := newV1Server(t, &auth.Authenticator{})

	rec := callV1(s, http.MethodGet, "/v1/uploads", map[string]string{
		"Authorization": "Bearer not-a-real-token",
	})

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401, body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/problem+json") {
		t.Errorf("Content-Type = %q, want application/problem+json", got)
	}
	if !strings.Contains(rec.Body.String(), "bearer auth not configured") {
		t.Errorf("body should explain misconfiguration, got: %s", rec.Body.String())
	}
}

// TestV1Route_APIKeyWithUnconfiguredValidator dispatches the API-key
// path of the auth middleware to a validator that is nil. The
// middleware must respond 401 with a problem+json body. This is the
// API-key counterpart of the bearer test above; together they prove
// the dispatch table covers both credential types.
func TestV1Route_APIKeyWithUnconfiguredValidator(t *testing.T) {
	s := newV1Server(t, &auth.Authenticator{})

	rec := callV1(s, http.MethodGet, "/v1/uploads", map[string]string{
		"X-API-Key": "ak_live_abcdefghijklmnop",
	})

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401, body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/problem+json") {
		t.Errorf("Content-Type = %q, want application/problem+json", got)
	}
	if !strings.Contains(rec.Body.String(), "api key auth not configured") {
		t.Errorf("body should explain misconfiguration, got: %s", rec.Body.String())
	}
}

// TestV1Route_BearerWithEmptyJWKS wires a real KeycloakVerifier
// whose JWKS set is empty. The verifier runs, cannot find a key
// matching the token's `kid`, and returns an error; the middleware
// translates that into 401. This is the more thorough proof that
// the authn path is reached: the test crosses from the dispatch
// layer into the actual verifier (the previous two tests stop at
// the dispatch layer because the verifier is nil).
func TestV1Route_BearerWithEmptyJWKS(t *testing.T) {
	cfg := &config.Config{
		KeycloakURL:      "https://kc.test",
		KeycloakRealm:    "orpheus",
		KeycloakClientID: "orpheus-api",
	}
	verifier, err := auth.NewKeycloakVerifierWithSet(jwk.NewSet(), cfg)
	if err != nil {
		t.Fatalf("NewKeycloakVerifierWithSet: %v", err)
	}
	s := newV1Server(t, &auth.Authenticator{Keycloak: verifier})

	rec := callV1(s, http.MethodGet, "/v1/uploads", map[string]string{
		"Authorization": "Bearer not-a-real-jwt",
	})

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401, body=%s", rec.Code, rec.Body.String())
	}
}

// TestV1Route_BearerVerifiesAndReachesHandler drives a real JWT
// through the middleware so the principal reaches the handler. The
// handler dereferences the (nil) *db.DB pool; chi's Recoverer
// middleware converts the resulting panic into a 500 response. A
// 500 here proves the request passed the auth middleware and
// entered the handler — anything earlier (no creds, bad token)
// would have surfaced as 401, not 500. This is the closest we
// can get to a happy-path smoke through the HTTP transport
// without external services.
func TestV1Route_BearerVerifiesAndReachesHandler(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}
	privKey, err := jwk.FromRaw(priv)
	if err != nil {
		t.Fatalf("jwk.FromRaw: %v", err)
	}
	_ = privKey.Set(jwk.KeyIDKey, "test-kid")
	_ = privKey.Set(jwk.AlgorithmKey, jwa.RS256)
	pubKey, err := privKey.PublicKey()
	if err != nil {
		t.Fatalf("PublicKey: %v", err)
	}
	set := jwk.NewSet()
	if err := set.AddKey(pubKey); err != nil {
		t.Fatalf("set.AddKey: %v", err)
	}
	cfg := &config.Config{
		KeycloakURL:      "https://kc.test",
		KeycloakRealm:    "orpheus",
		KeycloakClientID: "orpheus-api",
	}
	verifier, err := auth.NewKeycloakVerifierWithSet(set, cfg)
	if err != nil {
		t.Fatalf("NewKeycloakVerifierWithSet: %v", err)
	}

	tok, err := jwt.NewBuilder().
		Issuer("https://kc.test/realms/orpheus").
		Audience([]string{"orpheus-api"}).
		Subject("user-uuid-1").
		JwtID("jti-1").
		Build()
	if err != nil {
		t.Fatalf("jwt.NewBuilder.Build: %v", err)
	}
	signed, err := jwt.Sign(tok, jwt.WithKey(jwa.RS256, privKey))
	if err != nil {
		t.Fatalf("jwt.Sign: %v", err)
	}

	s := newV1Server(t, &auth.Authenticator{Keycloak: verifier})
	req := httptest.NewRequest(http.MethodGet, "/v1/uploads", nil)
	req.Header.Set("Authorization", "Bearer "+string(signed))
	rec := httptest.NewRecorder()

	s.mux.ServeHTTP(rec, req)

	if rec.Code == http.StatusUnauthorized {
		t.Fatalf("status = 401, want non-401 (token reached handler); body=%s", rec.Body.String())
	}
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (handler panicked on nil DB; Recoverer converted)", rec.Code)
	}
}
