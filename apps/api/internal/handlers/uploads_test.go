package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/orpheus/api/internal/auth"
)

// ─────────────────────────────────────────────────────────────────────
// writeJSON / writeProblem / nullStringVal
// ─────────────────────────────────────────────────────────────────────

func TestWriteJSON(t *testing.T) {
	rec := httptest.NewRecorder()
	writeJSON(rec, http.StatusCreated, map[string]string{"hello": "world"})

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusCreated)
	}
	if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	var got map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["hello"] != "world" {
		t.Errorf("body[hello] = %q, want world", got["hello"])
	}
}

func TestWriteProblem(t *testing.T) {
	rec := httptest.NewRecorder()
	writeProblem(rec, http.StatusNotFound, "not_found", "thing missing")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "application/problem+json") {
		t.Errorf("Content-Type = %q, want application/problem+json", got)
	}
	var got map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["status"].(float64) != float64(http.StatusNotFound) {
		t.Errorf("status field = %v, want %d", got["status"], http.StatusNotFound)
	}
	if got["detail"] != "thing missing" {
		t.Errorf("detail = %v, want 'thing missing'", got["detail"])
	}
	if !strings.HasPrefix(got["type"].(string), "https://docs.orpheus.dev/errors/") {
		t.Errorf("type = %v, want docs URL", got["type"])
	}
}

func TestNullStringVal(t *testing.T) {
	cases := []struct {
		name string
		in   *string
		want string
	}{
		{"nil pointer", nil, ""},
		{"empty string", strPtr(""), ""},
		{"value", strPtr("hello"), "hello"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := nullStringVal(tc.in); got != tc.want {
				t.Errorf("nullStringVal(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────
// Upload.Create validation (pure unit tests — no DB / S3 touched)
// ─────────────────────────────────────────────────────────────────────

// withPrincipal attaches a Principal to the request context, matching
// what the auth middleware would do in production. The values are
// only used by handlers that get past validation.
func withPrincipal(r *http.Request, p *auth.Principal) *http.Request {
	return r.WithContext(auth.WithPrincipal(r.Context(), p))
}

func TestUploadCreate_RejectsMissingFilename(t *testing.T) {
	h := &UploadHandler{}
	body := bytes.NewBufferString(`{"filename":"","content_type":"audio/wav","size_bytes":1024}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/uploads", body)
	req = withPrincipal(req, &auth.Principal{OrgID: "00000000-0000-0000-0000-000000000001", UserID: "00000000-0000-0000-0000-000000000002"})
	rec := httptest.NewRecorder()

	h.Create(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	if !strings.Contains(rec.Header().Get("Content-Type"), "application/problem+json") {
		t.Errorf("Content-Type = %q, want application/problem+json", rec.Header().Get("Content-Type"))
	}
}

func TestUploadCreate_RejectsOversize(t *testing.T) {
	h := &UploadHandler{}
	// 2 GiB — over the 1 GiB cap.
	body := bytes.NewBufferString(`{"filename":"x.bin","content_type":"audio/wav","size_bytes":2147483648}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/uploads", body)
	req = withPrincipal(req, &auth.Principal{OrgID: "00000000-0000-0000-0000-000000000001"})
	rec := httptest.NewRecorder()

	h.Create(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusRequestEntityTooLarge)
	}
}

func TestUploadCreate_RejectsZeroSize(t *testing.T) {
	h := &UploadHandler{}
	body := bytes.NewBufferString(`{"filename":"x.bin","content_type":"audio/wav","size_bytes":0}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/uploads", body)
	req = withPrincipal(req, &auth.Principal{OrgID: "00000000-0000-0000-0000-000000000001"})
	rec := httptest.NewRecorder()

	h.Create(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusRequestEntityTooLarge)
	}
}

func TestUploadCreate_RejectsUnauthenticated(t *testing.T) {
	h := &UploadHandler{}
	body := bytes.NewBufferString(`{"filename":"x.bin","content_type":"audio/wav","size_bytes":1024}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/uploads", body)
	// Deliberately no principal attached.
	rec := httptest.NewRecorder()

	h.Create(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func strPtr(s string) *string { return &s }
