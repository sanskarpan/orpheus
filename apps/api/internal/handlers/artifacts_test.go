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

// testArtifactDB opens a *tenant-scoped* pool against
// ORPHEUS_TEST_DATABASE_URL — the System Under Test connection. It does
// NOT set app.is_service, so the RLS policies are load-bearing: queries
// only see rows whose org_id matches the app.current_org_id that
// WithTenant sets. This is what lets the cross-tenant isolation tests
// (e.g. WrongOrgIs404) actually exercise RLS instead of bypassing it.
// Skips the test when the env var is not set.
func testArtifactDB(t *testing.T) *db.DB {
	return openTestPool(t, false)
}

// testServiceDB opens a service-role pool (app.is_service = 'true' on
// every connection) used ONLY to seed fixtures across arbitrary orgs
// without tripping RLS. Never use it as the SUT connection.
func testServiceDB(t *testing.T) *db.DB {
	return openTestPool(t, true)
}

func openTestPool(t *testing.T, service bool) *db.DB {
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
	if service {
		cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
			_, err := conn.Exec(ctx, "SET app.is_service = 'true'")
			return err
		}
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("pgxpool.NewWithConfig: %v", err)
	}
	t.Cleanup(pool.Close)
	return &db.DB{Pool: pool}
}

// seedOrgAndArtifacts inserts an org and n fully-probed artifacts using a
// dedicated service-role pool, so seeding works regardless of the SUT's
// tenant scope. Returns the org id and artifact ids.
func seedOrgAndArtifacts(t *testing.T, n int) (orgID string, artifactIDs []string) {
	pool := testServiceDB(t)
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
	orgID, want := seedOrgAndArtifacts(t, 3)

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

	missingID := uuid.NewString()
	req := httptest.NewRequest(http.MethodGet, "/v1/artifacts/"+missingID, nil)
	req = withPrincipal(req, &auth.Principal{OrgID: orgID})
	req = withURLParam(req, "id", missingID)
	rec := httptest.NewRecorder()

	h.Get(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body=%s", rec.Code, rec.Body.String())
	}
}

func TestArtifactGet_HappyPath(t *testing.T) {
	pool := testArtifactDB(t)
	orgID, ids := seedOrgAndArtifacts(t, 1)
	wantID := ids[0]

	h := &ArtifactHandler{DB: pool}
	req := httptest.NewRequest(http.MethodGet, "/v1/artifacts/"+wantID, nil)
	req = withPrincipal(req, &auth.Principal{OrgID: orgID})
	req = withURLParam(req, "id", wantID)
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
	_, ids := seedOrgAndArtifacts(t, 1)
	otherOrg := uuid.NewString()

	h := &ArtifactHandler{DB: pool}
	req := httptest.NewRequest(http.MethodGet, "/v1/artifacts/"+ids[0], nil)
	req = withPrincipal(req, &auth.Principal{OrgID: otherOrg})
	req = withURLParam(req, "id", ids[0])
	rec := httptest.NewRecorder()

	h.Get(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body=%s", rec.Code, rec.Body.String())
	}
}

// TestArtifactGet_UnprobedReturnsZeroValues is the regression test for
// the NULL-scan bug: a freshly-uploaded artifact has NULL codec,
// duration_seconds, sample_rate and channels (the probe worker has not
// run yet). Get must return 200 with those fields zero-valued, not 500.
func TestArtifactGet_UnprobedReturnsZeroValues(t *testing.T) {
	sut := testArtifactDB(t)
	svc := testServiceDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	orgID := uuid.NewString()
	artID := uuid.NewString()
	if _, err := svc.Exec(ctx, `INSERT INTO organizations (id, name, slug) VALUES ($1, $2, $2)`, orgID, "unprobed-"+orgID); err != nil {
		t.Fatalf("insert org: %v", err)
	}
	t.Cleanup(func() { _, _ = svc.Exec(context.Background(), `DELETE FROM organizations WHERE id = $1`, orgID) })
	// No codec/duration/sample_rate/channels → they stay NULL.
	if _, err := svc.Exec(ctx, `
		INSERT INTO artifacts (id, org_id, s3_bucket, s3_key, sha256, size_bytes, content_type)
		VALUES ($1, $2, 'b', $3, '', 10, 'audio/wav')
	`, artID, orgID, "k/"+artID); err != nil {
		t.Fatalf("insert artifact: %v", err)
	}

	h := &ArtifactHandler{DB: sut}
	req := httptest.NewRequest(http.MethodGet, "/v1/artifacts/"+artID, nil)
	req = withPrincipal(req, &auth.Principal{OrgID: orgID})
	req = withURLParam(req, "id", artID)
	rec := httptest.NewRecorder()

	h.Get(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var got Artifact
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ID != artID || got.Codec != "" || got.DurationSeconds != 0 || got.SampleRate != 0 || got.Channels != 0 {
		t.Errorf("unexpected artifact: %+v", got)
	}
}
