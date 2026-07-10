package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/orpheus/api/internal/auth"
	"github.com/orpheus/api/internal/db"
)

// testArtifactDB opens a service-role pool against
// ORPHEUS_TEST_DATABASE_URL. Every connection runs with
// `app.is_service = 'true'` so the test can read/write under any
// org_id without tripping the RLS policies. Skips the test when the
// env var is not set.
func testArtifactDB(t *testing.T) *db.DB {
	t.Helper()
	dsn := os.Getenv("ORPHEUS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ORPHEUS_TEST_DATABASE_URL not set; skipping live-db artifact tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("pgxpool.ParseConfig: %v", err)
	}
	cfg.MaxConns = 4
	cfg.MinConns = 1
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SET app.is_service = 'true'")
		return err
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("pgxpool.NewWithConfig: %v", err)
	}
	t.Cleanup(pool.Close)
	return &db.DB{Pool: pool}
}

func seedOrgAndArtifacts(t *testing.T, pool *db.DB, n int) (orgID string, artifactIDs []string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	orgID = uuid.NewString()
	if _, err := pool.Exec(ctx,
		`INSERT INTO organizations (id, name, slug) VALUES ($1, $2, $3)`,
		orgID, "test-"+orgID, "test-"+orgID,
	); err != nil {
		t.Fatalf("insert org: %v", err)
	}

	artifactIDs = make([]string, 0, n)
	for i := 0; i < n; i++ {
		id := uuid.NewString()
		if _, err := pool.Exec(ctx, `
			INSERT INTO artifacts (id, org_id, s3_bucket, s3_key, sha256, size_bytes, content_type, codec, duration_seconds, sample_rate, channels)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		`, id, orgID, "test-bucket", "test-key/"+id, "deadbeef", 1024, "audio/wav", "pcm_s16le", 1.5, 16000, 1); err != nil {
			t.Fatalf("insert artifact %d: %v", i, err)
		}
		artifactIDs = append(artifactIDs, id)
	}

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = pool.Exec(ctx, `DELETE FROM organizations WHERE id = $1`, orgID)
	})
	return orgID, artifactIDs
}

func TestArtifactList_EmptyOrg(t *testing.T) {
	pool := testArtifactDB(t)
	h := &ArtifactHandler{DB: pool}
	orgID := uuid.NewString()

	req := httptest.NewRequest(http.MethodGet, "/v1/artifacts", nil)
	req = withPrincipal(req, &auth.Principal{OrgID: orgID})
	rec := httptest.NewRecorder()

	h.List(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var got ArtifactList
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Data) != 0 {
		t.Errorf("len(Data) = %d, want 0", len(got.Data))
	}
	if got.HasMore {
		t.Errorf("HasMore = true, want false")
	}
	if got.NextCursor != "" {
		t.Errorf("NextCursor = %q, want empty", got.NextCursor)
	}
}

func TestArtifactList_WithRows(t *testing.T) {
	pool := testArtifactDB(t)
	orgID, want := seedOrgAndArtifacts(t, pool, 3)

	h := &ArtifactHandler{DB: pool}
	req := httptest.NewRequest(http.MethodGet, "/v1/artifacts", nil)
	req = withPrincipal(req, &auth.Principal{OrgID: orgID})
	rec := httptest.NewRecorder()

	h.List(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var got ArtifactList
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Data) != len(want) {
		t.Fatalf("len(Data) = %d, want %d", len(got.Data), len(want))
	}
	gotIDs := make(map[string]bool, len(got.Data))
	for _, a := range got.Data {
		gotIDs[a.ID] = true
		if a.ContentType != "audio/wav" {
			t.Errorf("artifact %q: ContentType = %q, want audio/wav", a.ID, a.ContentType)
		}
		if a.SHA256 != "deadbeef" {
			t.Errorf("artifact %q: SHA256 = %q, want deadbeef", a.ID, a.SHA256)
		}
	}
	for _, id := range want {
		if !gotIDs[id] {
			t.Errorf("artifact %q missing from response", id)
		}
	}
}

func TestArtifactGet_NotFound(t *testing.T) {
	pool := testArtifactDB(t)
	h := &ArtifactHandler{DB: pool}
	orgID := uuid.NewString()

	req := httptest.NewRequest(http.MethodGet, "/v1/artifacts/"+uuid.NewString(), nil)
	req = withPrincipal(req, &auth.Principal{OrgID: orgID})
	rec := httptest.NewRecorder()

	h.Get(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body=%s", rec.Code, rec.Body.String())
	}
}

func TestArtifactGet_HappyPath(t *testing.T) {
	pool := testArtifactDB(t)
	orgID, ids := seedOrgAndArtifacts(t, pool, 1)
	wantID := ids[0]

	h := &ArtifactHandler{DB: pool}
	req := httptest.NewRequest(http.MethodGet, "/v1/artifacts/"+wantID, nil)
	req = withPrincipal(req, &auth.Principal{OrgID: orgID})
	rec := httptest.NewRecorder()

	h.Get(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var got Artifact
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ID != wantID {
		t.Errorf("ID = %q, want %q", got.ID, wantID)
	}
	if got.SHA256 != "deadbeef" {
		t.Errorf("SHA256 = %q, want deadbeef", got.SHA256)
	}
	if got.SizeBytes != 1024 {
		t.Errorf("SizeBytes = %d, want 1024", got.SizeBytes)
	}
	if got.ContentType != "audio/wav" {
		t.Errorf("ContentType = %q, want audio/wav", got.ContentType)
	}
	if got.Codec != "pcm_s16le" {
		t.Errorf("Codec = %q, want pcm_s16le", got.Codec)
	}
	if got.SampleRate != 16000 {
		t.Errorf("SampleRate = %d, want 16000", got.SampleRate)
	}
	if got.Channels != 1 {
		t.Errorf("Channels = %d, want 1", got.Channels)
	}
}

func TestArtifactGet_WrongOrgIs404(t *testing.T) {
	pool := testArtifactDB(t)
	_, ids := seedOrgAndArtifacts(t, pool, 1)
	otherOrg := uuid.NewString()

	h := &ArtifactHandler{DB: pool}
	req := httptest.NewRequest(http.MethodGet, "/v1/artifacts/"+ids[0], nil)
	req = withPrincipal(req, &auth.Principal{OrgID: otherOrg})
	rec := httptest.NewRecorder()

	h.Get(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body=%s", rec.Code, rec.Body.String())
	}
}
