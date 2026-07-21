package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/alexedwards/argon2id"
)

// TestOnboarding_Provision provisions a tenant end-to-end and asserts the org,
// user, and a working API key were created.
func TestOnboarding_Provision(t *testing.T) {
	dsn := os.Getenv("ORPHEUS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ORPHEUS_TEST_DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sut := testArtifactDB(t)
	svc := testServiceDB(t)
	h := &OnboardingHandler{DB: sut}

	var createdOrg string
	t.Cleanup(func() {
		if createdOrg == "" {
			return
		}
		c, cc := context.WithTimeout(context.Background(), 10*time.Second)
		defer cc()
		_, _ = svc.Exec(c, `DELETE FROM api_keys WHERE org_id=$1`, createdOrg)
		_, _ = svc.Exec(c, `DELETE FROM users WHERE org_id=$1`, createdOrg)
		_, _ = svc.Exec(c, `DELETE FROM organizations WHERE id=$1`, createdOrg)
	})

	// Provision with default scopes.
	rec := httptest.NewRecorder()
	body := `{"org_name":"Acme, Inc.","user_email":"founder@acme.example"}`
	h.Provision(rec, httptest.NewRequest(http.MethodPost, "/v1/onboarding/provision", bytes.NewBufferString(body)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("Provision = %d; %s", rec.Code, rec.Body.String())
	}
	var resp ProvisionResponse
	_ = json.NewDecoder(rec.Body).Decode(&resp)
	createdOrg = resp.OrgID
	if resp.OrgID == "" || resp.UserID == "" || resp.APIKey == "" {
		t.Fatalf("incomplete response: %+v", resp)
	}
	if len(resp.Scopes) == 0 {
		t.Fatalf("no scopes returned")
	}

	// The org + user rows exist.
	var orgName, userEmail string
	if err := svc.QueryRow(ctx, `SELECT name FROM organizations WHERE id=$1`, resp.OrgID).Scan(&orgName); err != nil {
		t.Fatalf("org not found: %v", err)
	}
	if err := svc.QueryRow(ctx, `SELECT email FROM users WHERE id=$1`, resp.UserID).Scan(&userEmail); err != nil {
		t.Fatalf("user not found: %v", err)
	}
	if orgName != "Acme, Inc." || userEmail != "founder@acme.example" {
		t.Fatalf("org/user = %q / %q", orgName, userEmail)
	}

	// The returned API key is real: look it up by prefix and verify the hash.
	var hashed string
	var scopes []string
	if err := svc.QueryRow(ctx, `SELECT hashed_secret, scopes FROM api_keys WHERE org_id=$1`, resp.OrgID).Scan(&hashed, &scopes); err != nil {
		t.Fatalf("api key not found: %v", err)
	}
	ok, err := argon2id.ComparePasswordAndHash(resp.APIKey, hashed)
	if err != nil || !ok {
		t.Fatalf("returned key does not verify against stored hash (ok=%v err=%v)", ok, err)
	}
	if len(scopes) == 0 {
		t.Fatalf("key has no scopes")
	}

	// Validation: unknown scope → 400.
	rec = httptest.NewRecorder()
	h.Provision(rec, httptest.NewRequest(http.MethodPost, "/v1/onboarding/provision",
		bytes.NewBufferString(`{"org_name":"X","user_email":"a@b.co","scopes":["not:a:scope"]}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown scope = %d, want 400", rec.Code)
	}

	// Security: onboarding must not mint a wildcard or admin key.
	for _, bad := range []string{"*", "platform:admin"} {
		rec = httptest.NewRecorder()
		h.Provision(rec, httptest.NewRequest(http.MethodPost, "/v1/onboarding/provision",
			bytes.NewBufferString(`{"org_name":"X","user_email":"a@b.co","scopes":["`+bad+`"]}`)))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("scope %q = %d, want 400 (must not be grantable)", bad, rec.Code)
		}
	}

	// Validation: missing email → 400.
	rec = httptest.NewRecorder()
	h.Provision(rec, httptest.NewRequest(http.MethodPost, "/v1/onboarding/provision",
		bytes.NewBufferString(`{"org_name":"X"}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing email = %d, want 400", rec.Code)
	}
}
