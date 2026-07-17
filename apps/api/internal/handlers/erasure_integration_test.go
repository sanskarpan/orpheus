package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/orpheus/api/internal/audit"
	"github.com/orpheus/api/internal/auth"
)

// TestErasure_CreateGuardsAndFlow drives the erasure request API: confirm is
// required, a legal hold blocks, and a valid request is scheduled + fetchable.
func TestErasure_CreateGuardsAndFlow(t *testing.T) {
	sut := testArtifactDB(t)
	svc := testServiceDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	orgID := uuid.NewString()
	if _, err := svc.Exec(ctx, `INSERT INTO organizations (id,name,slug) VALUES ($1,$2,$2)`, orgID, "erz-"+orgID); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	artID := uuid.NewString()
	heldID := uuid.NewString()
	_, _ = svc.Exec(ctx, `INSERT INTO artifacts (id,org_id,s3_bucket,s3_key,sha256,size_bytes,content_type) VALUES ($1,$2,'b','k/e1',$3,5,'audio/wav')`, artID, orgID, "sha1")
	_, _ = svc.Exec(ctx, `INSERT INTO artifacts (id,org_id,s3_bucket,s3_key,sha256,size_bytes,content_type,legal_hold) VALUES ($1,$2,'b','k/e2',$3,5,'audio/wav',true)`, heldID, orgID, "sha2")
	t.Cleanup(func() {
		c, cc := context.WithTimeout(context.Background(), 5*time.Second)
		defer cc()
		_, _ = svc.Exec(c, `DELETE FROM erasure_requests WHERE org_id=$1`, orgID)
		_, _ = svc.Exec(c, `DELETE FROM artifacts WHERE org_id=$1`, orgID)
		_, _ = svc.Exec(c, `DELETE FROM organizations WHERE id=$1`, orgID)
	})

	h := &ErasureHandler{DB: sut, Audit: audit.New(sut, nil)}
	princ := &auth.Principal{OrgID: orgID}
	post := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.Create(rec, withPrincipal(httptest.NewRequest(http.MethodPost, "/v1/erasure-requests", bytes.NewReader([]byte(body))), princ))
		return rec
	}

	// confirm required
	if r := post(`{"scope":"artifact","artifact_id":"` + artID + `"}`); r.Code != http.StatusBadRequest {
		t.Fatalf("no-confirm = %d, want 400", r.Code)
	}
	// legal hold blocks
	if r := post(`{"scope":"artifact","artifact_id":"` + heldID + `","confirm":true}`); r.Code != http.StatusConflict {
		t.Fatalf("legal-hold = %d, want 409", r.Code)
	}
	// valid → scheduled
	rec := post(`{"scope":"artifact","artifact_id":"` + artID + `","reason":"gdpr_art17","confirm":true}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	var ev ErasureView
	_ = json.NewDecoder(rec.Body).Decode(&ev)
	if ev.Status != "scheduled" {
		t.Fatalf("status = %q, want scheduled", ev.Status)
	}

	// Get returns it.
	gr := chi.NewRouter()
	gr.Get("/v1/erasure-requests/{id}", func(w http.ResponseWriter, r *http.Request) { h.Get(w, withPrincipal(r, princ)) })
	grec := httptest.NewRecorder()
	gr.ServeHTTP(grec, httptest.NewRequest(http.MethodGet, "/v1/erasure-requests/"+ev.ID, nil))
	if grec.Code != http.StatusOK {
		t.Fatalf("get = %d", grec.Code)
	}

	// List includes it.
	lrec := httptest.NewRecorder()
	h.List(lrec, withPrincipal(httptest.NewRequest(http.MethodGet, "/v1/erasure-requests", nil), princ))
	var list struct {
		Data []ErasureView `json:"data"`
	}
	_ = json.NewDecoder(lrec.Body).Decode(&list)
	if len(list.Data) != 1 {
		t.Fatalf("list = %d, want 1", len(list.Data))
	}
}
