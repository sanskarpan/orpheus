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

	"github.com/google/uuid"

	"github.com/orpheus/api/internal/auth"
)

// TestMarketplace_SubmitReviewPromote drives a community-processor submission
// through moderation and asserts an approval promotes it into the public
// catalog as a community-trust processor.
func TestMarketplace_SubmitReviewPromote(t *testing.T) {
	dsn := os.Getenv("ORPHEUS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ORPHEUS_TEST_DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sut := testArtifactDB(t)
	svc := testServiceDB(t)
	orgID := uuid.NewString()
	if _, err := svc.Exec(ctx, `INSERT INTO organizations (id, name, slug) VALUES ($1,$2,$2)`, orgID, "mkt-"+orgID); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	procName := "community.demo-" + orgID[:8]
	t.Cleanup(func() {
		c, cc := context.WithTimeout(context.Background(), 10*time.Second)
		defer cc()
		_, _ = svc.Exec(c, `DELETE FROM processors WHERE name=$1`, procName)
		_, _ = svc.Exec(c, `DELETE FROM marketplace_submissions WHERE org_id=$1`, orgID)
		_, _ = svc.Exec(c, `DELETE FROM organizations WHERE id=$1`, orgID)
	})

	h := &MarketplaceHandler{DB: sut}
	principal := &auth.Principal{OrgID: orgID}

	// First-party processors are trust_class=first_party.
	lp := httptest.NewRecorder()
	h.ListProcessors(lp, withPrincipal(httptest.NewRequest(http.MethodGet, "/v1/marketplace/processors?trust_class=first_party", nil), principal))
	if lp.Code != http.StatusOK {
		t.Fatalf("ListProcessors = %d; %s", lp.Code, lp.Body.String())
	}

	// Submit a community processor.
	body := `{"name":"` + procName + `","display_name":"Demo","description":"a community processor","publisher":"acme"}`
	srec := httptest.NewRecorder()
	h.Submit(srec, withPrincipal(httptest.NewRequest(http.MethodPost, "/v1/marketplace/submissions", bytes.NewBufferString(body)), principal))
	if srec.Code != http.StatusCreated {
		t.Fatalf("Submit = %d; %s", srec.Code, srec.Body.String())
	}
	var sub struct {
		ID string `json:"id"`
	}
	_ = json.NewDecoder(srec.Body).Decode(&sub)

	// It appears in the org's submissions as pending.
	lsrec := httptest.NewRecorder()
	h.ListSubmissions(lsrec, withPrincipal(httptest.NewRequest(http.MethodGet, "/v1/marketplace/submissions", nil), principal))
	var subs struct {
		Data []MarketplaceSubmission `json:"data"`
	}
	_ = json.NewDecoder(lsrec.Body).Decode(&subs)
	if len(subs.Data) != 1 || subs.Data[0].Status != "pending" {
		t.Fatalf("submissions = %+v, want one pending", subs.Data)
	}

	// Approve → promotes into the catalog.
	rrec := httptest.NewRecorder()
	h.Review(rrec, withURLParam(httptest.NewRequest(http.MethodPost, "/v1/marketplace/submissions/"+sub.ID+"/review", bytes.NewBufferString(`{"decision":"approve","notes":"lgtm"}`)), "id", sub.ID))
	if rrec.Code != http.StatusOK {
		t.Fatalf("Review approve = %d; %s", rrec.Code, rrec.Body.String())
	}

	// The processor is now in the public catalog with community trust.
	lc := httptest.NewRecorder()
	h.ListProcessors(lc, withPrincipal(httptest.NewRequest(http.MethodGet, "/v1/marketplace/processors?trust_class=community", nil), principal))
	var cat struct {
		Data []MarketplaceProcessor `json:"data"`
	}
	_ = json.NewDecoder(lc.Body).Decode(&cat)
	found := false
	for _, m := range cat.Data {
		if m.Name == procName {
			found = true
			if m.TrustClass != "community" || m.Publisher != "acme" {
				t.Fatalf("promoted processor = %+v, want community/acme", m)
			}
		}
	}
	if !found {
		t.Fatalf("approved processor %q not in community catalog: %+v", procName, cat.Data)
	}

	// Re-reviewing is a conflict.
	r2 := httptest.NewRecorder()
	h.Review(r2, withURLParam(httptest.NewRequest(http.MethodPost, "/v1/marketplace/submissions/"+sub.ID+"/review", bytes.NewBufferString(`{"decision":"reject"}`)), "id", sub.ID))
	if r2.Code != http.StatusConflict {
		t.Fatalf("re-review = %d, want 409", r2.Code)
	}

	// Reviewing a missing submission is 404.
	r3 := httptest.NewRecorder()
	h.Review(r3, withURLParam(httptest.NewRequest(http.MethodPost, "/v1/marketplace/submissions/"+uuid.NewString()+"/review", bytes.NewBufferString(`{"decision":"approve"}`)), "id", uuid.NewString()))
	if r3.Code != http.StatusNotFound {
		t.Fatalf("review missing = %d, want 404", r3.Code)
	}
}
