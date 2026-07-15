// Package handlers — API key endpoints.
//
// An API key is a long-lived machine credential scoped to one org.
// The full secret is shown to the caller exactly once at creation;
// subsequent responses only return the metadata (id, name, prefix,
// scopes, last_used_at) plus a short, non-secret prefix that can
// appear in logs and the dashboard.
//
// Secrets are Argon2id-hashed at rest. We never store the cleartext
// and never re-display it after the create call, so a database leak
// does not yield usable credentials. The auth layer verifies the
// secret by Argon2id comparison in internal/auth/apikey.go.
//
// The full key shape is `ak_live_<random>`. The first 9 characters
// (`ak_live_X`) are the prefix used as a lookup index.
package handlers

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/alexedwards/argon2id"
	"github.com/google/uuid"

	"github.com/orpheus/api/internal/audit"
	"github.com/orpheus/api/internal/auth"
	"github.com/orpheus/api/internal/db"
	"github.com/orpheus/api/internal/dbtx"
)

// APIKeyHandler bundles the dependencies the API-key endpoints need.
type APIKeyHandler struct {
	DB    *db.DB
	Audit *audit.Recorder
}

// CreateAPIKeyRequest is the request body for POST /v1/api-keys.
// `ExpiresAt` is part of the OpenAPI surface but the Phase 1 schema
// does not yet persist it; it is accepted so future migrations can
// land without a wire-format change.
type CreateAPIKeyRequest struct {
	Name      string     `json:"name"`
	Scopes    []string   `json:"scopes"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// APIKey is the response shape for POST /v1/api-keys (with the
// cleartext secret populated) and GET /v1/api-keys (with the
// secret always empty).
type APIKey struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Prefix    string    `json:"prefix"`
	Scopes    []string  `json:"scopes"`
	CreatedAt time.Time `json:"created_at"`
	Secret    string    `json:"secret,omitempty"`
}

// APIKeyList is a cursor-paginated list of API keys (no secrets).
type APIKeyList struct {
	Data    []APIKey `json:"data"`
	HasMore bool     `json:"has_more"`
}

// Create handles POST /v1/api-keys. It generates a 32-byte random
// secret, base64-url-encodes it, stores the Argon2id hash in the DB,
// and returns the cleartext secret to the caller. The cleartext is
// never logged and never re-displayed.
func (h *APIKeyHandler) Create(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFromContext(r.Context())
	var req CreateAPIKeyRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, "validation", "Invalid JSON")
		return
	}
	if req.Name == "" {
		writeProblem(w, http.StatusBadRequest, "validation", "name required")
		return
	}
	if len(req.Scopes) == 0 {
		writeProblem(w, http.StatusBadRequest, "validation", "at least one scope is required")
		return
	}
	for _, s := range req.Scopes {
		if !auth.IsValidScope(s) {
			writeProblem(w, http.StatusBadRequest, "validation", "invalid scope: "+s)
			return
		}
		// Prevent privilege escalation: a caller cannot grant a scope it
		// does not itself hold (JWT/interactive users have full org
		// authority; a narrow API key cannot mint a broader one).
		if !p.CanGrantScope(s) {
			writeProblem(w, http.StatusForbidden, "forbidden", "cannot grant scope not held by the caller: "+s)
			return
		}
	}

	body := make([]byte, 32)
	if _, err := rand.Read(body); err != nil {
		writeProblem(w, http.StatusInternalServerError, "internal", "rand failed")
		return
	}
	secret := "ak_live_" + base64.RawURLEncoding.EncodeToString(body)
	prefix := secret[:9]

	hashed, err := argon2id.CreateHash(secret, argon2id.DefaultParams)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "internal", "hash failed")
		return
	}

	id := uuid.NewString()
	now := time.Now()
	err = h.DB.WithTenant(r.Context(), p.OrgID, func(ctx context.Context) error {
		_, err := dbtx.Exec(ctx, h.DB, `
			INSERT INTO api_keys (id, org_id, name, hashed_secret, prefix, scopes, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
		`, id, p.OrgID, req.Name, hashed, prefix, req.Scopes, now)
		return err
	})
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "internal", "Failed to create key")
		return
	}

	_ = h.Audit.Record(r.Context(), audit.Entry{
		Action:       "apikey.create",
		ResourceType: "api_key",
		ResourceID:   id,
		Metadata:     map[string]any{"name": req.Name, "scopes": req.Scopes},
	})

	writeJSON(w, http.StatusCreated, APIKey{
		ID:        id,
		Name:      req.Name,
		Prefix:    prefix,
		Scopes:    req.Scopes,
		CreatedAt: now,
		Secret:    secret,
	})
}

// List handles GET /v1/api-keys. Revoked keys are excluded. The
// cleartext secret is never returned — the Secret field is left
// empty so the wire format matches the one in Create exactly.
func (h *APIKeyHandler) List(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFromContext(r.Context())
	limit := 50
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 && n <= 200 {
			limit = n
		}
	}

	args := []any{p.OrgID, limit + 1}
	query := `SELECT id, name, prefix, scopes, created_at FROM api_keys
			  WHERE org_id = $1 AND revoked_at IS NULL ORDER BY created_at DESC LIMIT $2`

	var keys []APIKey
	err := h.DB.WithTenant(r.Context(), p.OrgID, func(ctx context.Context) error {
		rows, err := dbtx.Query(ctx, h.DB, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var k APIKey
			if err := rows.Scan(&k.ID, &k.Name, &k.Prefix, &k.Scopes, &k.CreatedAt); err != nil {
				return err
			}
			keys = append(keys, k)
		}
		return rows.Err()
	})
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "internal", "Failed to list keys")
		return
	}

	hasMore := len(keys) > limit
	if hasMore {
		keys = keys[:limit]
	}

	writeJSON(w, http.StatusOK, APIKeyList{Data: keys, HasMore: hasMore})
}

// Revoke handles DELETE /v1/api-keys/{id}. It marks the key revoked
// by setting revoked_at = now(); subsequent authentications will
// reject the prefix even if the secret would otherwise match. The
// endpoint is idempotent: revoking an already-revoked or unknown
// key both return 204 so the client can retry freely.
func (h *APIKeyHandler) Revoke(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFromContext(r.Context())
	id, ok := uuidParam(r, "id")
	if !ok {
		// Revoke is idempotent: an unknown or malformed id is a no-op 204,
		// not a cast error surfaced as 500.
		w.WriteHeader(http.StatusNoContent)
		return
	}

	err := h.DB.WithTenant(r.Context(), p.OrgID, func(ctx context.Context) error {
		_, err := dbtx.Exec(ctx, h.DB, `UPDATE api_keys SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL`, id)
		return err
	})
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "internal", "Failed to revoke key")
		return
	}

	_ = h.Audit.Record(r.Context(), audit.Entry{
		Action:       "apikey.revoke",
		ResourceType: "api_key",
		ResourceID:   id,
	})

	w.WriteHeader(http.StatusNoContent)
}
