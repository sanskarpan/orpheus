package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/orpheus/api/internal/auth"
)

// GetAPIKeyByPrefix implements the lookup half of
// auth.APIKeyLookup. The SQL mirrors the GetAPIKeyByPrefix query in
// internal/db/queries/api_keys.sql verbatim; it lives here as a
// hand-written method so the auth middleware can run before sqlc is
// regenerated. When sqlc is wired into the build pipeline this method
// can be deleted and callers can use the generated function instead.
//
// The prefix is the only piece of the key we can use to find the row
// before verifying the hashed_secret. We intentionally do NOT scope
// to org_id; the caller resolves the org from the row itself.
func (db *DB) GetAPIKeyByPrefix(ctx context.Context, prefix string) (auth.APIKeyRecord, error) {
	if db == nil {
		return auth.APIKeyRecord{}, errors.New("db.apikey.nil_pool")
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
		LIMIT 1
	`
	// The lookup runs BEFORE any tenant context exists (we resolve the org
	// from the row), so it must run as the service role — under FORCE ROW
	// LEVEL SECURITY the api_keys_tenant_select policy
	// (is_service_role() OR org_id = current_org_id()) otherwise filters the
	// row out and every API-key request 401s. Scope the GUC to a short tx so
	// pooled connections don't leak service-role privilege.
	conn, err := db.Acquire(ctx)
	if err != nil {
		return auth.APIKeyRecord{}, fmt.Errorf("db.apikey.acquire: %w", err)
	}
	defer conn.Release()
	tx, err := conn.Begin(ctx)
	if err != nil {
		return auth.APIKeyRecord{}, fmt.Errorf("db.apikey.begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT set_config('app.is_service','true',true)"); err != nil {
		return auth.APIKeyRecord{}, fmt.Errorf("db.apikey.service_role: %w", err)
	}

	var rec auth.APIKeyRecord
	var revoked *string
	err = tx.QueryRow(ctx, q, prefix).Scan(
		&rec.ID,
		&rec.OrgID,
		&rec.HashedSecret,
		&rec.Prefix,
		&rec.Scopes,
		&revoked,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return auth.APIKeyRecord{}, errors.New("db.apikey.not_found")
		}
		return auth.APIKeyRecord{}, err
	}
	rec.RevokedAt = revoked
	return rec, nil
}
