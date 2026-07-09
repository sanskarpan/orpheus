package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWriteJSON_SetsContentTypeAndStatus(t *testing.T) {
	rec := httptest.NewRecorder()
	writeJSON(rec, http.StatusTeapot, map[string]string{"hello": "world"})

	if rec.Code != http.StatusTeapot {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusTeapot)
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	var body map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["hello"] != "world" {
		t.Errorf("body[hello] = %q, want world", body["hello"])
	}
}

func TestWriteProblem_EmitsProblemJSON(t *testing.T) {
	rec := httptest.NewRecorder()
	writeProblem(rec, http.StatusNotFound, "not_found", "thing missing")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/problem+json") {
		t.Errorf("Content-Type = %q, want application/problem+json", got)
	}

	var body struct {
		Type   string `json:"type"`
		Title  string `json:"title"`
		Status int    `json:"status"`
		Detail string `json:"detail"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Status != 404 {
		t.Errorf("body.status = %d, want 404", body.Status)
	}
	if body.Detail != "thing missing" {
		t.Errorf("body.detail = %q, want thing missing", body.Detail)
	}
	if !strings.HasSuffix(body.Type, "not_found") {
		t.Errorf("body.type = %q, want suffix not_found", body.Type)
	}
}

func TestNullStringVal_DerefsOrEmpty(t *testing.T) {
	s := "value"
	if got := nullStringVal(&s); got != "value" {
		t.Errorf("non-nil = %q, want value", got)
	}
	if got := nullStringVal(nil); got != "" {
		t.Errorf("nil = %q, want empty", got)
	}
}

func TestMergeAndExtractProcessor_RoundTrip(t *testing.T) {
	original := ProcessorRef{Name: "whisper-transcribe", Version: "1.2.0"}
	raw := []byte(`{"language":"en","diarize":true}`)

	stored := mergeProcessorIntoParams(raw, original)
	got := extractProcessorFromParams(stored)
	if got != original {
		t.Errorf("extract(%s) = %+v, want %+v", stored, got, original)
	}

	// Strip the key: the result should equal the original user params
	// up to JSON key ordering. Compare as decoded maps so key order
	// does not matter.
	stripped := stripProcessorFromParams(stored)
	var strippedMap, originalMap map[string]any
	if err := json.Unmarshal(stripped, &strippedMap); err != nil {
		t.Fatalf("strip is not a JSON object: %v", err)
	}
	if err := json.Unmarshal(raw, &originalMap); err != nil {
		t.Fatalf("raw is not a JSON object: %v", err)
	}
	if len(strippedMap) != len(originalMap) {
		t.Errorf("stripped has %d keys, want %d", len(strippedMap), len(originalMap))
	}
	for k, v := range originalMap {
		if strippedMap[k] != v {
			t.Errorf("stripped[%q] = %v, want %v", k, strippedMap[k], v)
		}
	}
}

func TestExtractProcessor_HandlesEmptyAndMalformed(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
	}{
		{"empty", nil},
		{"not json", []byte(`{not json`)},
		{"no key", []byte(`{"language":"en"}`)},
		{"not object", []byte(`[]`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractProcessorFromParams(tc.in); got != (ProcessorRef{}) {
				t.Errorf("got %+v, want zero", got)
			}
		})
	}
}

func TestStripProcessorFromParams_PreservesNonObject(t *testing.T) {
	raw := []byte(`["a","b"]`)
	got := stripProcessorFromParams(raw)
	if string(got) != string(raw) {
		t.Errorf("got %s, want %s", got, raw)
	}
}
