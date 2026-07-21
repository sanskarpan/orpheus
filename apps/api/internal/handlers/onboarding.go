// Package handlers — customer onboarding (Phase 6).
//
// Provision a new tenant in one call: create the org, a first user, and an
// initial API key (returned once, in cleartext) so a new customer goes from
// zero to "can submit a job". This is a platform-admin provisioning primitive
// (the route requires the "platform:admin" role via auth.RequireRole, which a
// normal org JWT does NOT auto-satisfy) — self-serve signup with email
// verification is a follow-up; an unauthenticated org-creation endpoint would
// be an abuse vector. The minted key is least-privilege: onboarding refuses to
// grant "*" or "platform:admin".
package handlers

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/mail"
	"regexp"
	"strings"

	"github.com/alexedwards/argon2id"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/orpheus/api/internal/audit"
	"github.com/orpheus/api/internal/auth"
	"github.com/orpheus/api/internal/db"
)

type OnboardingHandler struct {
	DB    *db.DB
	Audit *audit.Recorder
}

type ProvisionRequest struct {
	OrgName   string   `json:"org_name"`
	UserEmail string   `json:"user_email"`
	KeyName   string   `json:"key_name,omitempty"`
	Scopes    []string `json:"scopes,omitempty"`
}

type ProvisionResponse struct {
	OrgID     string   `json:"org_id"`
	UserID    string   `json:"user_id"`
	APIKey    string   `json:"api_key"` // cleartext — shown exactly once
	KeyPrefix string   `json:"key_prefix"`
	Scopes    []string `json:"scopes"`
}

var slugStrip = regexp.MustCompile(`[^a-z0-9]+`)

// defaultOnboardingScopes let a new tenant immediately upload + run jobs.
var defaultOnboardingScopes = []string{"jobs:write", "jobs:read", "uploads:write", "uploads:read", "artifacts:read"}

// Provision creates an org + first user + initial API key atomically.
func (h *OnboardingHandler) Provision(w http.ResponseWriter, r *http.Request) {
	var req ProvisionRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, "validation", "Invalid JSON")
		return
	}
	if strings.TrimSpace(req.OrgName) == "" {
		writeProblem(w, http.StatusBadRequest, "validation", "org_name is required")
		return
	}
	if len(req.OrgName) > 200 {
		writeProblem(w, http.StatusBadRequest, "validation", "org_name too long")
		return
	}
	if _, err := mail.ParseAddress(req.UserEmail); err != nil {
		writeProblem(w, http.StatusBadRequest, "validation", "a valid user_email is required")
		return
	}
	scopes := req.Scopes
	if len(scopes) == 0 {
		scopes = defaultOnboardingScopes
	}
	for _, s := range scopes {
		if !auth.IsValidScope(s) {
			writeProblem(w, http.StatusBadRequest, "validation", "unknown scope: "+s)
			return
		}
		// Onboarding mints least-privilege keys: it must never grant the
		// wildcard or the platform-admin scope (privilege escalation).
		if s == "*" || s == "platform:admin" {
			writeProblem(w, http.StatusBadRequest, "validation", "scope not grantable via onboarding: "+s)
			return
		}
	}
	keyName := req.KeyName
	if keyName == "" {
		keyName = "default"
	}

	// Generate the key (same shape as APIKeyHandler.Create): ak_live_<random>.
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		writeProblem(w, http.StatusInternalServerError, "internal", "Failed to generate key")
		return
	}
	secret := "ak_live_" + base64.RawURLEncoding.EncodeToString(raw)
	prefix := secret[:9]
	hashed, err := argon2id.CreateHash(secret, argon2id.DefaultParams)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "internal", "Failed to hash key")
		return
	}

	orgID := uuid.NewString()
	userID := uuid.NewString()
	slug := slugify(req.OrgName, orgID)

	// Cross-tenant provisioning runs service-role (creating a brand-new org).
	err = h.withServiceTx(r.Context(), func(tx pgx.Tx) error {
		if _, e := tx.Exec(r.Context(),
			`INSERT INTO organizations (id, name, slug) VALUES ($1, $2, $3)`, orgID, req.OrgName, slug); e != nil {
			return e
		}
		if _, e := tx.Exec(r.Context(),
			`INSERT INTO users (id, org_id, email, name) VALUES ($1, $2, $3, $4)`, userID, orgID, req.UserEmail, req.UserEmail); e != nil {
			return e
		}
		_, e := tx.Exec(r.Context(),
			`INSERT INTO api_keys (id, org_id, name, hashed_secret, prefix, scopes) VALUES ($1, $2, $3, $4, $5, $6)`,
			uuid.NewString(), orgID, keyName, hashed, prefix, scopes)
		return e
	})
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "internal", "Failed to provision tenant")
		return
	}
	if h.Audit != nil {
		// Credential-and-tenant-minting op — record it (key prefix only, never
		// the secret). Actor is the calling platform admin.
		_ = h.Audit.Record(r.Context(), audit.Entry{
			OrgID: orgID, Action: "org.create", ResourceType: "org", ResourceID: orgID,
			Metadata: map[string]any{"key_prefix": prefix, "user_id": userID},
		})
	}
	writeJSON(w, http.StatusCreated, ProvisionResponse{
		OrgID: orgID, UserID: userID, APIKey: secret, KeyPrefix: prefix, Scopes: scopes,
	})
}

func slugify(name, orgID string) string {
	s := slugStrip.ReplaceAllString(strings.ToLower(name), "-")
	s = strings.Trim(s, "-")
	if s == "" {
		s = "org"
	}
	if len(s) > 40 {
		s = s[:40]
	}
	// Suffix keeps slugs unique without a retry loop.
	return s + "-" + orgID[:8]
}

func (h *OnboardingHandler) withServiceTx(ctx context.Context, fn func(pgx.Tx) error) error {
	conn, err := h.DB.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()
	if _, err := tx.Exec(ctx, "SELECT set_config('app.is_service','true',true)"); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	committed = true
	return nil
}
