package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/orpheus/api/internal/auth"
)

// TestAPIKeyCreate_ScopeRules covers ISSUE-6: scope validation and the
// no-privilege-escalation subset check. All cases fail before any DB
// access, so a nil-DB handler is sufficient.
func TestAPIKeyCreate_ScopeRules(t *testing.T) {
	cases := []struct {
		name     string
		caller   *auth.Principal
		body     string
		wantCode int
	}{
		{
			name:     "missing scopes rejected",
			caller:   &auth.Principal{OrgID: "o", UserID: "u"},
			body:     `{"name":"k","scopes":[]}`,
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "unknown scope rejected",
			caller:   &auth.Principal{OrgID: "o", UserID: "u"},
			body:     `{"name":"k","scopes":["jobs:destroy"]}`,
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "api key cannot escalate to wildcard",
			caller:   &auth.Principal{OrgID: "o", APIKeyID: "ak", Roles: []string{"jobs:read"}},
			body:     `{"name":"k","scopes":["*"]}`,
			wantCode: http.StatusForbidden,
		},
		{
			name:     "api key cannot grant scope it lacks",
			caller:   &auth.Principal{OrgID: "o", APIKeyID: "ak", Roles: []string{"jobs:read"}},
			body:     `{"name":"k","scopes":["webhooks:write"]}`,
			wantCode: http.StatusForbidden,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &APIKeyHandler{} // nil DB: all cases reject before DB use
			req := httptest.NewRequest(http.MethodPost, "/v1/api-keys", strings.NewReader(tc.body))
			req = withPrincipal(req, tc.caller)
			rec := httptest.NewRecorder()
			h.Create(rec, req)
			if rec.Code != tc.wantCode {
				t.Fatalf("code = %d, want %d (body=%s)", rec.Code, tc.wantCode, rec.Body.String())
			}
		})
	}
}
