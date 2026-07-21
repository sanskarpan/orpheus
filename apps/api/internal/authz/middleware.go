package authz

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/orpheus/api/internal/auth"
)

// Require returns middleware that authorizes the request against the policy for
// the given required scope. It mirrors auth.RequireScope's contract (JWT users
// pass, API keys need the scope or "*") but routes the decision through Rego,
// so deny-overrides and future rules apply uniformly. It must be mounted under
// the auth middleware.
//
// A policy-evaluation error fails closed (403) — an authorization layer that
// errors open would be a security hole.
func (a *Authorizer) Require(scope string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			in := Input{RequiredScope: scope, Write: isWrite(r.Method)}
			if p, err := auth.PrincipalFromContext(r.Context()); err == nil && p != nil {
				in.Principal = PrincipalInput{
					Type:         principalType(p),
					Scopes:       p.Roles,
					OrgSuspended: p.OrgSuspended,
				}
			} else {
				in.Principal = PrincipalInput{Type: "anonymous"}
			}

			d, err := a.Authorize(r.Context(), in)
			if err != nil {
				slog.Error("authz.eval_failed", "err", err, "scope", scope)
				writeForbidden(w, scope, nil)
				return
			}
			if !d.Allow {
				writeForbidden(w, scope, d.Deny)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func principalType(p *auth.Principal) string {
	if p.APIKeyID == "" {
		return "jwt" // interactive user
	}
	return "apikey"
}

func isWrite(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

func writeForbidden(w http.ResponseWriter, scope string, deny []string) {
	detail := "missing required scope: " + scope
	if len(deny) > 0 {
		detail = "denied by policy: " + strings.Join(deny, ", ")
	}
	// Marshal the body so a deny reason containing a quote/control char can
	// never break or inject into the JSON (defence in depth on the authz path).
	body, _ := json.Marshal(map[string]any{
		"type":   "https://docs.orpheus.dev/errors/forbidden",
		"title":  "Forbidden",
		"status": http.StatusForbidden,
		"detail": detail,
	})
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(http.StatusForbidden)
	_, _ = w.Write(body)
}
