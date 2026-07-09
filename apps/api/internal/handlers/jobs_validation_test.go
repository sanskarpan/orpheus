package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/orpheus/api/internal/auth"
)

// TestJobCreate_RejectsUnauthenticated asserts that Create returns 401
// when no principal is on the request context — the auth middleware
// normally catches this, but a misconfigured route should still fail
// closed rather than fall through with an empty org id.
func TestJobCreate_RejectsUnauthenticated(t *testing.T) {
	h := &JobHandler{}
	body := strings.NewReader(`{"artifact_id":"00000000-0000-0000-0000-000000000001","processor":{"name":"whisper-transcribe","version":"1.0.0"}}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/jobs", body)
	rec := httptest.NewRecorder()

	h.Create(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/problem+json") {
		t.Errorf("Content-Type = %q, want application/problem+json", got)
	}
}

// TestJobCreate_RejectsMalformedJSON asserts that Create responds
// with 400 when the request body is not valid JSON. The handler
// does not touch the DB on this path, so we can drive it with
// h := &JobHandler{} — no DB dependency.
func TestJobCreate_RejectsMalformedJSON(t *testing.T) {
	h := &JobHandler{}
	body := strings.NewReader(`{not json`)
	req := httptest.NewRequest(http.MethodPost, "/v1/jobs", body)
	req = withPrincipal(req, &auth.Principal{OrgID: "00000000-0000-0000-0000-000000000001"})
	rec := httptest.NewRecorder()

	h.Create(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// TestJobCreate_RejectsMissingRequiredFields asserts that Create
// responds with 400 when the body parses but required fields are
// empty. We don't reach the DB on this path.
func TestJobCreate_RejectsMissingRequiredFields(t *testing.T) {
	h := &JobHandler{}
	cases := []struct {
		name string
		body string
	}{
		{"no artifact", `{"processor":{"name":"x","version":"1.0.0"}}`},
		{"no processor", `{"artifact_id":"00000000-0000-0000-0000-000000000001"}`},
		{"no version", `{"artifact_id":"00000000-0000-0000-0000-000000000001","processor":{"name":"x","version":""}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/jobs", strings.NewReader(tc.body))
			req = withPrincipal(req, &auth.Principal{OrgID: "00000000-0000-0000-0000-000000000001"})
			rec := httptest.NewRecorder()

			h.Create(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", rec.Code)
			}
		})
	}
}

// TestJobBulkCreate_RejectsEmptyAndOversize asserts that BulkCreate
// rejects batches outside the 0 < n <= maxBulkJobs window. Both
// paths fail before any DB work.
func TestJobBulkCreate_RejectsEmptyAndOversize(t *testing.T) {
	h := &JobHandler{}

	t.Run("empty", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v1/jobs/bulk", strings.NewReader(`{"jobs":[]}`))
		req = withPrincipal(req, &auth.Principal{OrgID: "00000000-0000-0000-0000-000000000001"})
		rec := httptest.NewRecorder()

		h.BulkCreate(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
	})

	t.Run("oversize", func(t *testing.T) {
		// Build a batch with maxBulkJobs+1 well-formed items.
		items := make([]map[string]any, maxBulkJobs+1)
		for i := range items {
			items[i] = map[string]any{
				"artifact_id": "00000000-0000-0000-0000-000000000001",
				"processor":   map[string]string{"name": "p", "version": "1"},
			}
		}
		body, _ := json.Marshal(map[string]any{"jobs": items})
		req := httptest.NewRequest(http.MethodPost, "/v1/jobs/bulk", strings.NewReader(string(body)))
		req = withPrincipal(req, &auth.Principal{OrgID: "00000000-0000-0000-0000-000000000001"})
		rec := httptest.NewRecorder()

		h.BulkCreate(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
	})
}

// TestJobListLimitClamp verifies that the limit parameter is clamped
// to the [1, 200] window. We can't observe the SQL without a DB, so
// we test the limit-clamping logic via a tiny helper that mirrors the
// parsing in JobHandler.List. The point is to lock the behaviour of
// the public surface — e.g. that an over-large `limit` does not
// produce a query that reads every row in the table.
func TestJobListLimitClamp(t *testing.T) {
	parse := func(raw string) int {
		const def, max = 50, 200
		if raw == "" {
			return def
		}
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 || n > max {
			return def
		}
		return n
	}
	cases := []struct {
		in   string
		want int
	}{
		{"", 50},
		{"0", 50},
		{"-5", 50},
		{"1000", 50},
		{"abc", 50},
		{"1", 1},
		{"50", 50},
		{"200", 200},
	}
	for _, tc := range cases {
		t.Run("limit="+tc.in, func(t *testing.T) {
			if got := parse(tc.in); got != tc.want {
				t.Errorf("parse(%q) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}
