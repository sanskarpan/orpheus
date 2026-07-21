package authz

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/orpheus/api/internal/auth"
)

func newAuthorizer(t *testing.T) *Authorizer {
	t.Helper()
	a, err := New(context.Background())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

func TestAuthorize_Decisions(t *testing.T) {
	a := newAuthorizer(t)
	cases := []struct {
		name      string
		in        Input
		wantAllow bool
		wantDeny  string
	}{
		{
			name:      "apikey with required scope",
			in:        Input{Principal: PrincipalInput{Type: "apikey", Scopes: []string{"jobs:write"}}, RequiredScope: "jobs:write", Write: true},
			wantAllow: true,
		},
		{
			name:      "apikey missing scope",
			in:        Input{Principal: PrincipalInput{Type: "apikey", Scopes: []string{"jobs:read"}}, RequiredScope: "jobs:write", Write: true},
			wantAllow: false,
		},
		{
			name:      "apikey wildcard",
			in:        Input{Principal: PrincipalInput{Type: "apikey", Scopes: []string{"*"}}, RequiredScope: "data:erase", Write: true},
			wantAllow: true,
		},
		{
			name:      "jwt has full authority",
			in:        Input{Principal: PrincipalInput{Type: "jwt"}, RequiredScope: "jobs:write", Write: true},
			wantAllow: true,
		},
		{
			name:      "anonymous denied",
			in:        Input{Principal: PrincipalInput{Type: "anonymous"}, RequiredScope: "jobs:read", Write: false},
			wantAllow: false,
		},
		{
			// Parity with auth.RequireScope(""): a scoped API key does NOT pass
			// an empty-scope route (only a "*" key or a JWT does). Guards the
			// B1 privilege-escalation regression.
			name:      "empty required scope denies a scoped apikey",
			in:        Input{Principal: PrincipalInput{Type: "apikey", Scopes: []string{"jobs:read"}}, RequiredScope: "", Write: false},
			wantAllow: false,
		},
		{
			name:      "empty required scope allows a wildcard apikey",
			in:        Input{Principal: PrincipalInput{Type: "apikey", Scopes: []string{"*"}}, RequiredScope: "", Write: false},
			wantAllow: true,
		},
		{
			name:      "empty required scope allows a jwt",
			in:        Input{Principal: PrincipalInput{Type: "jwt"}, RequiredScope: "", Write: false},
			wantAllow: true,
		},
		{
			name:      "suspended org may read",
			in:        Input{Principal: PrincipalInput{Type: "apikey", Scopes: []string{"jobs:read"}, OrgSuspended: true}, RequiredScope: "jobs:read", Write: false},
			wantAllow: true,
		},
		{
			name:      "suspended org may not write (deny override)",
			in:        Input{Principal: PrincipalInput{Type: "apikey", Scopes: []string{"jobs:write"}, OrgSuspended: true}, RequiredScope: "jobs:write", Write: true},
			wantAllow: false,
			wantDeny:  "org_suspended",
		},
		{
			name:      "suspended org: even a JWT cannot write",
			in:        Input{Principal: PrincipalInput{Type: "jwt", OrgSuspended: true}, RequiredScope: "jobs:write", Write: true},
			wantAllow: false,
			wantDeny:  "org_suspended",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, err := a.Authorize(context.Background(), tc.in)
			if err != nil {
				t.Fatalf("Authorize: %v", err)
			}
			if d.Allow != tc.wantAllow {
				t.Fatalf("allow = %v, want %v (deny=%v)", d.Allow, tc.wantAllow, d.Deny)
			}
			if tc.wantDeny != "" {
				found := false
				for _, r := range d.Deny {
					if r == tc.wantDeny {
						found = true
					}
				}
				if !found {
					t.Fatalf("deny = %v, want to contain %q", d.Deny, tc.wantDeny)
				}
			}
		})
	}
}

func TestRequireMiddleware(t *testing.T) {
	a := newAuthorizer(t)
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	// API key with the scope → 200.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/jobs", nil)
	req = req.WithContext(auth.WithPrincipal(req.Context(), &auth.Principal{OrgID: "o1", APIKeyID: "k1", Roles: []string{"jobs:write"}}))
	a.Require("jobs:write")(next).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("with scope = %d, want 200", rec.Code)
	}

	// API key without the scope → 403.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/v1/jobs", nil)
	req = req.WithContext(auth.WithPrincipal(req.Context(), &auth.Principal{OrgID: "o1", APIKeyID: "k1", Roles: []string{"jobs:read"}}))
	a.Require("jobs:write")(next).ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("missing scope = %d, want 403", rec.Code)
	}

	// JWT principal (no APIKeyID) → 200 regardless of scope.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/v1/jobs", nil)
	req = req.WithContext(auth.WithPrincipal(req.Context(), &auth.Principal{OrgID: "o1", UserID: "u1"}))
	a.Require("jobs:write")(next).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("jwt = %d, want 200", rec.Code)
	}

	// Suspended org, write → 403 (deny override) even with the scope.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodDelete, "/v1/jobs/x", nil)
	req = req.WithContext(auth.WithPrincipal(req.Context(), &auth.Principal{OrgID: "o1", APIKeyID: "k1", Roles: []string{"*"}, OrgSuspended: true}))
	a.Require("jobs:write")(next).ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("suspended write = %d, want 403", rec.Code)
	}

	// No principal → 403.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v1/jobs", nil)
	a.Require("jobs:read")(next).ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("anonymous = %d, want 403", rec.Code)
	}
}
