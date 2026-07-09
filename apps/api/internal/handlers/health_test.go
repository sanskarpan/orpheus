package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLiveness(t *testing.T) {
	tests := []struct {
		name           string
		wantStatus     int
		wantStatusBody string
	}{
		{
			name:           "returns ok",
			wantStatus:     http.StatusOK,
			wantStatusBody: "ok",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/health", nil)
			rec := httptest.NewRecorder()

			Liveness(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "application/json") {
				t.Errorf("Content-Type = %q, want contains application/json", got)
			}

			var body livenessResponse
			if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			if body.Status != tt.wantStatusBody {
				t.Errorf("body.status = %q, want %q", body.Status, tt.wantStatusBody)
			}
		})
	}
}

func TestReadiness(t *testing.T) {
	tests := []struct {
		name           string
		wantStatus     int
		wantStatusBody string
		wantChecks     map[string]string
	}{
		{
			name:           "returns ready with service check",
			wantStatus:     http.StatusOK,
			wantStatusBody: "ready",
			wantChecks:     map[string]string{"service": "ok"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/ready", nil)
			rec := httptest.NewRecorder()

			Readiness(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "application/json") {
				t.Errorf("Content-Type = %q, want contains application/json", got)
			}

			var body readinessResponse
			if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			if body.Status != tt.wantStatusBody {
				t.Errorf("body.status = %q, want %q", body.Status, tt.wantStatusBody)
			}
			if len(body.Checks) != len(tt.wantChecks) {
				t.Fatalf("len(Checks) = %d, want %d", len(body.Checks), len(tt.wantChecks))
			}
			for k, v := range tt.wantChecks {
				if got := body.Checks[k]; got != v {
					t.Errorf("body.Checks[%q] = %q, want %q", k, got, v)
				}
			}
		})
	}
}
