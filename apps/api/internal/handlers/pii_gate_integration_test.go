package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/orpheus/api/internal/auth"
	"github.com/orpheus/api/internal/config"
	"github.com/orpheus/api/internal/storage/s3"
)

// TestPII_MappingArtifactRequiresUnmaskScope proves a pii_mapping-sensitivity
// artifact's signed URL is gated by the pii:unmask scope (PRD 08).
func TestPII_MappingArtifactRequiresUnmaskScope(t *testing.T) {
	sut := testArtifactDB(t)
	svc := testServiceDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	s3c, err := s3.New(ctx, &config.Config{
		S3Endpoint: "http://127.0.0.1:9000", S3AccessKey: "orpheus",
		S3SecretKey: "orpheus-dev-secret", S3Bucket: "orpheus-uploads",
	})
	if err != nil {
		t.Skipf("s3 unavailable: %v", err)
	}

	orgID := uuid.NewString()
	if _, err := svc.Exec(ctx, `INSERT INTO organizations (id,name,slug) VALUES ($1,$2,$2)`, orgID, "pii-"+orgID); err != nil {
		t.Fatalf("seed org: %v", err)
	}
	mapID, normID := uuid.NewString(), uuid.NewString()
	_, _ = svc.Exec(ctx, `INSERT INTO artifacts (id,org_id,s3_bucket,s3_key,sha256,size_bytes,content_type,sensitivity) VALUES ($1,$2,'orpheus-uploads','pii-mappings/m.json',$3,10,'application/json','pii_mapping')`, mapID, orgID, "sha-m")
	_, _ = svc.Exec(ctx, `INSERT INTO artifacts (id,org_id,s3_bucket,s3_key,sha256,size_bytes,content_type) VALUES ($1,$2,'orpheus-uploads','k/n.wav',$3,10,'audio/wav')`, normID, orgID, "sha-n")
	t.Cleanup(func() {
		c, cc := context.WithTimeout(context.Background(), 5*time.Second)
		defer cc()
		_, _ = svc.Exec(c, `DELETE FROM artifacts WHERE org_id=$1`, orgID)
		_, _ = svc.Exec(c, `DELETE FROM organizations WHERE id=$1`, orgID)
	})

	h := &ArtifactHandler{DB: sut, S3: s3c}
	call := func(artifactID string, princ *auth.Principal) int {
		r := chi.NewRouter()
		r.Get("/v1/artifacts/{id}/signed-url", func(w http.ResponseWriter, req *http.Request) {
			h.GetSignedURL(w, withPrincipal(req, princ))
		})
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/artifacts/"+artifactID+"/signed-url", nil))
		return rec.Code
	}

	apiKeyNoScope := &auth.Principal{OrgID: orgID, APIKeyID: "k1", Roles: []string{"artifacts:read"}}
	apiKeyUnmask := &auth.Principal{OrgID: orgID, APIKeyID: "k2", Roles: []string{"artifacts:read", "pii:unmask"}}

	if code := call(mapID, apiKeyNoScope); code != http.StatusForbidden {
		t.Fatalf("pii_mapping without scope = %d, want 403", code)
	}
	if code := call(mapID, apiKeyUnmask); code != http.StatusOK {
		t.Fatalf("pii_mapping with pii:unmask = %d, want 200", code)
	}
	if code := call(normID, apiKeyNoScope); code != http.StatusOK {
		t.Fatalf("normal artifact = %d, want 200", code)
	}
}
