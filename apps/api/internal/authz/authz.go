// Package authz is the request-time authorization layer for the Orpheus API.
//
// It compiles an embedded Rego policy once at startup (via OPA's in-process
// rego engine — no external OPA server) and evaluates it per request. This
// sits alongside Postgres RLS: RLS isolates tenant *data*; this policy decides
// whether a principal may perform an *action* (scope enforcement plus
// extensible deny-overrides such as suspended orgs), as policy-as-code.
package authz

import (
	"context"
	_ "embed"
	"fmt"

	"github.com/open-policy-agent/opa/v1/rego"
)

//go:embed policy.rego
var policyModule string

// PrincipalInput is the principal projection the policy reasons over.
type PrincipalInput struct {
	Type         string   `json:"type"` // "jwt" | "apikey" | "anonymous"
	Scopes       []string `json:"scopes"`
	OrgSuspended bool     `json:"org_suspended"`
}

// Input is the full policy input for one authorization decision.
type Input struct {
	Principal     PrincipalInput `json:"principal"`
	RequiredScope string         `json:"required_scope"`
	Write         bool           `json:"write"`
}

// Decision is the policy result.
type Decision struct {
	Allow bool     `json:"allow"`
	Deny  []string `json:"deny"`
}

// Authorizer evaluates the compiled policy. Safe for concurrent use.
type Authorizer struct {
	query rego.PreparedEvalQuery
}

// New compiles the embedded policy and prepares the decision query. Failing to
// compile is a programming error (the policy is embedded), so callers should
// treat an error as fatal at startup.
func New(ctx context.Context) (*Authorizer, error) {
	q, err := rego.New(
		rego.Query("data.orpheus.authz.decision"),
		rego.Module("policy.rego", policyModule),
	).PrepareForEval(ctx)
	if err != nil {
		return nil, fmt.Errorf("authz: compile policy: %w", err)
	}
	return &Authorizer{query: q}, nil
}

// Authorize evaluates the policy for one request.
func (a *Authorizer) Authorize(ctx context.Context, in Input) (Decision, error) {
	rs, err := a.query.Eval(ctx, rego.EvalInput(in))
	if err != nil {
		return Decision{}, fmt.Errorf("authz: eval: %w", err)
	}
	if len(rs) == 0 || len(rs[0].Expressions) == 0 {
		// No decision produced — fail closed.
		return Decision{Allow: false}, nil
	}
	m, ok := rs[0].Expressions[0].Value.(map[string]any)
	if !ok {
		return Decision{Allow: false}, nil
	}
	d := Decision{}
	if allow, ok := m["allow"].(bool); ok {
		d.Allow = allow
	}
	if reasons, ok := m["deny"].([]any); ok {
		for _, r := range reasons {
			if s, ok := r.(string); ok {
				d.Deny = append(d.Deny, s)
			}
		}
	}
	return d, nil
}
