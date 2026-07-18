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

// TestStreamingSession_Lifecycle drives create → get → list → finalize through
// the handler against a real Postgres, asserting RLS scoping and that finalize
// persists the transcript + billable duration + cost.
func TestStreamingSession_Lifecycle(t *testing.T) {
	dsn := os.Getenv("ORPHEUS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ORPHEUS_TEST_DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sut := testArtifactDB(t)
	svc := testServiceDB(t)
	orgID := uuid.NewString()
	otherOrg := uuid.NewString()
	for _, o := range []string{orgID, otherOrg} {
		if _, err := svc.Exec(ctx, `INSERT INTO organizations (id, name, slug) VALUES ($1,$2,$2)`, o, "str-"+o); err != nil {
			t.Fatalf("seed org: %v", err)
		}
	}
	t.Cleanup(func() {
		c, cc := context.WithTimeout(context.Background(), 10*time.Second)
		defer cc()
		for _, o := range []string{orgID, otherOrg} {
			_, _ = svc.Exec(c, `DELETE FROM streaming_sessions WHERE org_id=$1`, o)
			_, _ = svc.Exec(c, `DELETE FROM organizations WHERE id=$1`, o)
		}
	})

	h := &StreamingHandler{DB: sut}
	principal := &auth.Principal{OrgID: orgID}

	// Create.
	crec := httptest.NewRecorder()
	h.Create(crec, withPrincipal(httptest.NewRequest(http.MethodPost, "/v1/streaming/sessions",
		bytes.NewBufferString(`{"model_version_id":"streaming-asr-1"}`)), principal))
	if crec.Code != http.StatusCreated {
		t.Fatalf("Create = %d, want 201; %s", crec.Code, crec.Body.String())
	}
	var created StreamingSession
	_ = json.NewDecoder(crec.Body).Decode(&created)
	if created.ID == "" || created.Status != "connecting" || created.WSURL == "" {
		t.Fatalf("bad create response: %+v", created)
	}

	// Get.
	grec := httptest.NewRecorder()
	h.Get(grec, withURLParam(withPrincipal(httptest.NewRequest(http.MethodGet, "/v1/streaming/sessions/"+created.ID, nil), principal), "id", created.ID))
	if grec.Code != http.StatusOK {
		t.Fatalf("Get = %d; %s", grec.Code, grec.Body.String())
	}

	// Cross-tenant Get must 404 (RLS).
	orec := httptest.NewRecorder()
	h.Get(orec, withURLParam(withPrincipal(httptest.NewRequest(http.MethodGet, "/v1/streaming/sessions/"+created.ID, nil), &auth.Principal{OrgID: otherOrg}), "id", created.ID))
	if orec.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant Get = %d, want 404 (RLS)", orec.Code)
	}

	// List.
	lrec := httptest.NewRecorder()
	h.List(lrec, withPrincipal(httptest.NewRequest(http.MethodGet, "/v1/streaming/sessions", nil), principal))
	var listResp struct {
		Data []StreamingSession `json:"data"`
	}
	_ = json.NewDecoder(lrec.Body).Decode(&listResp)
	if len(listResp.Data) != 1 || listResp.Data[0].ID != created.ID {
		t.Fatalf("List = %+v, want the one session", listResp.Data)
	}

	// Finalize.
	frec := httptest.NewRecorder()
	h.Finalize(frec, withURLParam(withPrincipal(httptest.NewRequest(http.MethodPost, "/v1/streaming/sessions/"+created.ID+"/finalize",
		bytes.NewBufferString(`{"transcript":"hello world","audio_seconds":30}`)), principal), "id", created.ID))
	if frec.Code != http.StatusOK {
		t.Fatalf("Finalize = %d; %s", frec.Code, frec.Body.String())
	}
	var finalized StreamingSession
	_ = json.NewDecoder(frec.Body).Decode(&finalized)
	if finalized.Status != "closed" {
		t.Fatalf("finalized status = %q, want closed", finalized.Status)
	}
	if finalized.Transcript == nil || *finalized.Transcript != "hello world" {
		t.Fatalf("transcript not persisted: %+v", finalized.Transcript)
	}
	if finalized.CostUSD <= 0 || finalized.AudioSeconds == nil || *finalized.AudioSeconds != 30 {
		t.Fatalf("billing not recorded: cost=%v audio=%v", finalized.CostUSD, finalized.AudioSeconds)
	}

	// Finalize again is idempotent (already closed → 200, not 404).
	f2 := httptest.NewRecorder()
	h.Finalize(f2, withURLParam(withPrincipal(httptest.NewRequest(http.MethodPost, "/v1/streaming/sessions/"+created.ID+"/finalize",
		bytes.NewBufferString(`{"transcript":"ignored","audio_seconds":0}`)), principal), "id", created.ID))
	if f2.Code != http.StatusOK {
		t.Fatalf("idempotent Finalize = %d, want 200", f2.Code)
	}
}
