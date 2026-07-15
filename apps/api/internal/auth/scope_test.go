package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRequireScope(t *testing.T) {
	cases := []struct {
		name     string
		p        *Principal
		scope    string
		wantCode int
	}{
		{"jwt user bypasses", &Principal{OrgID: "o", UserID: "u"}, "jobs:write", http.StatusOK},
		{"api key with scope", &Principal{OrgID: "o", APIKeyID: "k", Roles: []string{"jobs:write"}}, "jobs:write", http.StatusOK},
		{"api key with wildcard", &Principal{OrgID: "o", APIKeyID: "k", Roles: []string{"*"}}, "jobs:write", http.StatusOK},
		{"api key missing scope", &Principal{OrgID: "o", APIKeyID: "k", Roles: []string{"jobs:read"}}, "jobs:write", http.StatusForbidden},
		{"api key no scopes", &Principal{OrgID: "o", APIKeyID: "k"}, "jobs:read", http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
			h := RequireScope(tc.scope)(next)
			req := httptest.NewRequest(http.MethodGet, "/v1/jobs", nil)
			req = req.WithContext(WithPrincipal(req.Context(), tc.p))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.wantCode {
				t.Fatalf("code = %d, want %d", rec.Code, tc.wantCode)
			}
		})
	}
}

func TestRequireScope_NoPrincipalForbidden(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := RequireScope("jobs:read")(next)
	req := httptest.NewRequest(http.MethodGet, "/v1/jobs", nil) // no principal
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("code = %d, want 403", rec.Code)
	}
}
