// Command bootstrap-admin mints the first platform-admin API key for a local
// or fresh environment. The API has no seeder and every privileged endpoint
// (onboarding/provision, api-key management) already requires a privileged
// credential — a chicken-and-egg the dashboard's signup flow needs broken so
// it can provision tenants under the hood.
//
// It creates (idempotently) a dedicated "platform-operator" org and inserts an
// api_keys row scoped {"*","platform:admin"}, using the same Argon2id hashing
// and key shape as the API's own key issuance. The cleartext secret is printed
// once — store it as ORPHEUS_ADMIN_KEY for the web BFF.
//
// Usage:
//
//	bootstrap-admin                       # uses ORPHEUS_DATABASE_URL / ORPHEUS_TEST_DATABASE_URL
//	bootstrap-admin postgres://user:pw@host/db?sslmode=disable
package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"time"

	"github.com/alexedwards/argon2id"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const (
	orgName  = "Platform Operator"
	orgSlug  = "platform-operator"
	keyName  = "bootstrap-admin"
	adminEml = "platform-admin@orpheus.local"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "bootstrap-admin:", err)
		os.Exit(1)
	}
}

func run() error {
	dsn := os.Getenv("ORPHEUS_DATABASE_URL")
	if len(os.Args) > 1 {
		dsn = os.Args[1]
	} else if dsn == "" {
		dsn = os.Getenv("ORPHEUS_TEST_DATABASE_URL")
	}
	if dsn == "" {
		return fmt.Errorf("no DSN: pass one as an argument or set ORPHEUS_DATABASE_URL / ORPHEUS_TEST_DATABASE_URL")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	// Generate the credential (same shape as APIKeyHandler.Create / onboarding).
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return fmt.Errorf("rand: %w", err)
	}
	secret := "ak_live_" + base64.RawURLEncoding.EncodeToString(raw)
	prefix := secret[:9]
	hashed, err := argon2id.CreateHash(secret, argon2id.DefaultParams)
	if err != nil {
		return fmt.Errorf("hash: %w", err)
	}

	// All inserts run under the service role so forced RLS lets us seed a
	// brand-new org (matching OnboardingHandler.withServiceTx).
	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, "SELECT set_config('app.is_service','true',true)"); err != nil {
		return fmt.Errorf("set service role: %w", err)
	}

	var orgID string
	if err := tx.QueryRow(ctx,
		`INSERT INTO organizations (id, name, slug) VALUES ($1, $2, $3)
		 ON CONFLICT (slug) DO UPDATE SET name = EXCLUDED.name
		 RETURNING id`,
		uuid.NewString(), orgName, orgSlug,
	).Scan(&orgID); err != nil {
		return fmt.Errorf("upsert org: %w", err)
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO users (id, org_id, email, name) VALUES ($1, $2, $3, $4)
		 ON CONFLICT (org_id, email) DO NOTHING`,
		uuid.NewString(), orgID, adminEml, "Platform Admin",
	); err != nil {
		return fmt.Errorf("insert user: %w", err)
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO api_keys (id, org_id, name, hashed_secret, prefix, scopes)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		uuid.NewString(), orgID, keyName, hashed, prefix,
		[]string{"*", "platform:admin"},
	); err != nil {
		return fmt.Errorf("insert api_key: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}

	// Machine-readable last line (KEY=...) so a Make target can capture it.
	fmt.Fprintf(os.Stderr, "bootstrap-admin: minted platform-admin key for org %s (prefix %s)\n", orgID, prefix)
	fmt.Printf("%s\n", secret)
	return nil
}
