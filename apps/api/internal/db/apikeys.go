package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/orpheus/api/internal/auth"
)

// GetAPIKeysByPrefix implements the lookup half of auth.APIKeyLookup: it
// returns EVERY active key sharing a prefix. The stored prefix is only
// "ak_live_" + one base64 char, so collisions are expected; the caller
// Argon2-verifies the presented secret against each candidate. Returning a
// single row (the old LIMIT 1) made auth fail whenever a colliding key was
// picked instead of the requester's.
//
// The prefix is the only piece of the key we can use to find rows before
// verifying the hashed_secret. We intentionally do NOT scope to org_id; the
// caller resolves the org from the matching row itself.
func (db *DB) GetAPIKeysByPrefix(ctx context.Context, prefix string) ([]auth.APIKeyRecord, error) {
	if db == nil {
		return nil, errors.New("db.apikey.nil_pool")
	}
	const q = `
		SELECT id::text,
		       org_id::text,
		       hashed_secret,
		       prefix,
		       scopes,
		       revoked_at::text
		FROM api_keys
		WHERE prefix = $1
		  AND revoked_at IS NULL
	`
	// The lookup runs BEFORE any tenant context exists (we resolve the org
	// from the row), so it must run as the service role — under FORCE ROW
	// LEVEL SECURITY the api_keys_tenant_select policy
	// (is_service_role() OR org_id = current_org_id()) otherwise filters the
	// row out and every API-key request 401s. Scope the GUC to a short tx so
	// pooled connections don't leak service-role privilege.
	conn, err := db.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("db.apikey.acquire: %w", err)
	}
	defer conn.Release()
	tx, err := conn.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("db.apikey.begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT set_config('app.is_service','true',true)"); err != nil {
		return nil, fmt.Errorf("db.apikey.service_role: %w", err)
	}

	rows, err := tx.Query(ctx, q, prefix)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []auth.APIKeyRecord
	for rows.Next() {
		var rec auth.APIKeyRecord
		var revoked *string
		if err := rows.Scan(&rec.ID, &rec.OrgID, &rec.HashedSecret, &rec.Prefix, &rec.Scopes, &revoked); err != nil {
			return nil, err
		}
		rec.RevokedAt = revoked
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
