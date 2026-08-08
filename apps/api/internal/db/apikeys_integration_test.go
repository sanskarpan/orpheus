package db_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/orpheus/api/internal/db"
)

// TestGetAPIKeyByPrefix_UnderRLS is the regression test for the production bug
// where API-key auth was fully broken: GetAPIKeyByPrefix ran a plain query on
// a non-service connection, but api_keys has FORCE ROW LEVEL SECURITY with an
// `is_service_role() OR org_id = current_org_id()` SELECT policy — and auth
// runs BEFORE any tenant context exists, so the row was filtered out and every
// API-key request 401'd. The e2e harness masked it by using a service-role
// pool. This test uses a plain pool (RLS enforced, as in production) and would
// fail before the fix.
func TestGetAPIKeyByPrefix_UnderRLS(t *testing.T) {
	dsn := os.Getenv("ORPHEUS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ORPHEUS_TEST_DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// The system-under-test: a PLAIN pool (no service role on connect) — the
	// same shape main.go uses in production.
	sut, err := db.New(ctx, dsn)
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	t.Cleanup(sut.Close)

	// Seed an org + api key using an explicit service-role connection.
	seed, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("seed connect: %v", err)
	}
	defer func() { _ = seed.Close(ctx) }()
	if _, err := seed.Exec(ctx, "SET app.is_service = 'true'"); err != nil {
		t.Fatalf("seed service role: %v", err)
	}
	orgID := uuid.NewString()
	if _, err := seed.Exec(ctx, `INSERT INTO organizations (id,name,slug) VALUES ($1,$2,$3)`, orgID, "rls-"+orgID[:8], "rls-"+orgID[:8]); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	secret := "ak_live_" + base64.RawURLEncoding.EncodeToString(b)
	prefix := secret[:9]
	if _, err := seed.Exec(ctx,
		`INSERT INTO api_keys (id,org_id,name,hashed_secret,prefix,scopes) VALUES ($1,$2,'rls-test','$argon2id$fake',$3,$4)`,
		uuid.NewString(), orgID, prefix, []string{"jobs:read"}); err != nil {
		t.Fatalf("seed api_key: %v", err)
	}
	t.Cleanup(func() {
		c, cc := context.WithTimeout(context.Background(), 10*time.Second)
		defer cc()
		conn, e := pgx.Connect(c, dsn)
		if e != nil {
			return
		}
		defer func() { _ = conn.Close(c) }()
		_, _ = conn.Exec(c, "SET app.is_service = 'true'")
		_, _ = conn.Exec(c, `DELETE FROM api_keys WHERE org_id=$1`, orgID)
		_, _ = conn.Exec(c, `DELETE FROM organizations WHERE id=$1`, orgID)
	})

	// The lookup MUST find the key even though the SUT pool has no tenant
	// context — GetAPIKeyByPrefix has to elevate to the service role itself.
	rec, err := sut.GetAPIKeyByPrefix(ctx, prefix)
	if err != nil {
		t.Fatalf("GetAPIKeyByPrefix returned %v — API-key auth is broken under RLS", err)
	}
	if rec.Prefix != prefix || rec.OrgID != orgID {
		t.Fatalf("wrong record: got prefix=%q org=%q", rec.Prefix, rec.OrgID)
	}
}
