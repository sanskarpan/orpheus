package orpheus

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestJobs_CreateGetList(t *testing.T) {
	var gotKey, gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("X-API-Key")
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/jobs":
			gotCT = r.Header.Get("Content-Type")
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(Job{ID: "job-1", Status: "queued", Processor: ProcessorRef{Name: "transcribe", Version: "1.0.0"}})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/jobs/job-1":
			_ = json.NewEncoder(w).Encode(Job{ID: "job-1", Status: "completed"})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/jobs":
			if r.URL.Query().Get("limit") != "2" {
				t.Errorf("limit query = %q, want 2", r.URL.Query().Get("limit"))
			}
			_ = json.NewEncoder(w).Encode(Page[Job]{Data: []Job{{ID: "job-1"}, {ID: "job-2"}}, NextCursor: "c2"})
		default:
			http.Error(w, "unexpected", http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	c := New(srv.URL, WithAPIKey("ak_live_test"))
	ctx := context.Background()

	job, err := c.Jobs.Create(ctx, CreateJobRequest{ArtifactID: "a1", Processor: ProcessorRef{Name: "transcribe", Version: "1.0.0"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if job.ID != "job-1" || job.Status != "queued" {
		t.Fatalf("job = %+v", job)
	}
	if gotKey != "ak_live_test" {
		t.Fatalf("X-API-Key = %q", gotKey)
	}
	if gotCT != "application/json" {
		t.Fatalf("Content-Type = %q", gotCT)
	}

	got, err := c.Jobs.Get(ctx, "job-1")
	if err != nil || got.Status != "completed" {
		t.Fatalf("Get = %+v, %v", got, err)
	}

	page, err := c.Jobs.List(ctx, &ListOptions{Limit: 2})
	if err != nil || len(page.Data) != 2 || page.NextCursor != "c2" {
		t.Fatalf("List = %+v, %v", page, err)
	}
}

func TestJobs_WaitForCompletion(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		status := "running"
		if n >= 3 {
			status = "completed"
		}
		_ = json.NewEncoder(w).Encode(Job{ID: "job-1", Status: status})
	}))
	defer srv.Close()

	c := New(srv.URL, WithAPIKey("k"))
	job, err := c.Jobs.WaitForCompletion(context.Background(), "job-1", 5*time.Millisecond)
	if err != nil {
		t.Fatalf("WaitForCompletion: %v", err)
	}
	if job.Status != "completed" {
		t.Fatalf("status = %q, want completed", job.Status)
	}
	if atomic.LoadInt32(&calls) < 3 {
		t.Fatalf("expected >=3 polls, got %d", calls)
	}
}

func TestAPIError_Mapping(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"type":"https://docs.orpheus.dev/errors/not_found","title":"Not Found","status":404,"detail":"job not found","request_id":"req-9"}`))
	}))
	defer srv.Close()

	c := New(srv.URL, WithAPIKey("k"))
	_, err := c.Jobs.Get(context.Background(), "missing")
	if err == nil {
		t.Fatal("expected error")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error is not *APIError: %T", err)
	}
	if !apiErr.IsNotFound() || apiErr.Detail != "job not found" || apiErr.RequestID != "req-9" {
		t.Fatalf("apiErr = %+v", apiErr)
	}
}

func TestNoAPIKey_HeaderAbsent(t *testing.T) {
	var hadKey bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, hadKey = r.Header["X-Api-Key"]
		_ = json.NewEncoder(w).Encode(Job{ID: "j"})
	}))
	defer srv.Close()
	// No WithAPIKey option.
	if _, err := New(srv.URL).Jobs.Get(context.Background(), "j"); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if hadKey {
		t.Fatal("X-API-Key header should be absent when no key is configured")
	}
}

func TestMalformedErrorBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("upstream exploded, not json"))
	}))
	defer srv.Close()
	_, err := New(srv.URL).Jobs.Get(context.Background(), "j")
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("want *APIError, got %T", err)
	}
	if apiErr.Status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", apiErr.Status)
	}
	// Error() must still be actionable (falls back to Raw).
	if apiErr.Error() == "" || apiErr.Raw != "upstream exploded, not json" {
		t.Fatalf("bad fallback: %q / raw=%q", apiErr.Error(), apiErr.Raw)
	}
}

func TestWaitForCompletion_ContextCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(Job{ID: "j", Status: "running"}) // never completes
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := New(srv.URL, WithAPIKey("k")).Jobs.WaitForCompletion(ctx, "j", 5*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want context deadline error, got %v", err)
	}
}

func TestUploadsAndArtifacts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/uploads":
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(UploadSession{ID: "u1", Status: "pending", Parts: []UploadPart{{PartNumber: 1, URL: "https://s3/part1"}}})
		case r.URL.Path == "/v1/artifacts/art-1":
			_ = json.NewEncoder(w).Encode(Artifact{ID: "art-1", ContentType: "audio/wav", SizeBytes: 1234})
		default:
			http.Error(w, "unexpected", http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	c := New(srv.URL, WithAPIKey("k"))
	ctx := context.Background()
	u, err := c.Uploads.Create(ctx, CreateUploadRequest{Filename: "a.wav", ContentType: "audio/wav", SizeBytes: 100})
	if err != nil || u.ID != "u1" || len(u.Parts) != 1 {
		t.Fatalf("Uploads.Create = %+v, %v", u, err)
	}
	a, err := c.Artifacts.Get(ctx, "art-1")
	if err != nil || a.ContentType != "audio/wav" || a.SizeBytes != 1234 {
		t.Fatalf("Artifacts.Get = %+v, %v", a, err)
	}
}
