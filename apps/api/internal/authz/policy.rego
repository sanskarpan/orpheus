# Orpheus authorization policy (Phase 5: OPA/Rego alongside RLS).
#
# RLS is the tenant-isolation boundary in the database; this policy is the
# request-time authorization boundary. It decides whether a principal may
# perform an action, given the route's required scope, using policy-as-code so
# authz rules live in one auditable place and can grow deny-overrides without
# touching Go.
#
# Input shape (built by internal/authz/authz.go):
#   {
#     "principal": {
#       "type": "jwt" | "apikey" | "anonymous",
#       "scopes": ["jobs:write", ...],   # api-key scopes / jwt roles
#       "org_suspended": false
#     },
#     "required_scope": "jobs:write",
#     "write": true                       # mutating HTTP method?
#   }
#
# The Go side reads data.orpheus.authz.decision = {"allow": bool, "deny": [...]}.
package orpheus.authz

import rego.v1

# ── scope satisfaction ──────────────────────────────────────────────
# Interactive users (JWT) have full authority within their org (RLS still
# scopes their data). API keys must hold the route's scope or the wildcard.
scope_ok if input.principal.type == "jwt"

scope_ok if {
	input.principal.type == "apikey"
	input.required_scope in input.principal.scopes
}

scope_ok if {
	input.principal.type == "apikey"
	"*" in input.principal.scopes
}

# Note: there is deliberately NO "empty required_scope ⇒ any principal" rule.
# That would let a narrowly-scoped API key pass a route the in-Go
# auth.RequireScope would deny (which only passes JWTs or "*" keys on an empty
# scope). JWT principals already pass via the type=="jwt" rule above.

# ── deny overrides (win over any allow) ─────────────────────────────
# A suspended org may still read, but never mutate.
deny contains "org_suspended" if {
	input.principal.org_suspended == true
	input.write == true
}

# ── final decision ──────────────────────────────────────────────────
default allow := false

allow if {
	scope_ok
	count(deny) == 0
}

decision := {"allow": allow, "deny": deny}
