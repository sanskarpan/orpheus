package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestRequireRole verifies the admin gate does NOT auto-pass JWT principals
// (unlike RequireScope) — the role must be held explicitly.
func TestRequireRole(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mw := RequireRole("platform:admin")

	cases := []struct {
		name string
		p    *Principal
		want int
	}{
		{"jwt without the role is denied", &Principal{OrgID: "o1", UserID: "u1", Roles: []string{"user"}}, http.StatusForbidden},
		{"jwt with no roles is denied", &Principal{OrgID: "o1", UserID: "u1"}, http.StatusForbidden},
		{"jwt with the role passes", &Principal{OrgID: "o1", UserID: "u1", Roles: []string{"platform:admin"}}, http.StatusOK},
		{"apikey without the scope is denied", &Principal{OrgID: "o1", APIKeyID: "k1", Roles: []string{"jobs:write"}}, http.StatusForbidden},
		{"apikey with wildcard is denied (not the explicit role)", &Principal{OrgID: "o1", APIKeyID: "k1", Roles: []string{"*"}}, http.StatusForbidden},
		{"apikey with the scope passes", &Principal{OrgID: "o1", APIKeyID: "k1", Roles: []string{"platform:admin"}}, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/onboarding/provision", nil)
			req = req.WithContext(WithPrincipal(req.Context(), tc.p))
			mw(next).ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("code = %d, want %d", rec.Code, tc.want)
			}
		})
	}

	// No principal → denied.
	rec := httptest.NewRecorder()
	mw(next).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/x", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("no principal = %d, want 403", rec.Code)
	}
}
