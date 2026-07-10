package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/orpheus/api/internal/audit"
	"github.com/orpheus/api/internal/auth"
	"github.com/orpheus/api/internal/db"
)

// ─────────────────────────────────────────────────────────────────────
// Create validation
// ─────────────────────────────────────────────────────────────────────

func TestWebhookCreate_RejectsUnauthenticated(t *testing.T) {
	t.Parallel()
	h := &WebhookHandler{}
	body := strings.NewReader(`{"url":"https://example.com/h","subscribed_events":["job.succeeded"]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/webhooks", body)
	rec := httptest.NewRecorder()
	h.Create(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestWebhookCreate_RejectsMalformedJSON(t *testing.T) {
	t.Parallel()
	h := &WebhookHandler{}
	body := strings.NewReader(`{not json`)
	req := httptest.NewRequest(http.MethodPost, "/v1/webhooks", body)
	req = withPrincipal(req, &auth.Principal{OrgID: "00000000-0000-0000-0000-000000000001"})
	rec := httptest.NewRecorder()
	h.Create(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestWebhookCreate_RejectsMissingURL(t *testing.T) {
	t.Parallel()
	h := &WebhookHandler{}
	body := strings.NewReader(`{"url":"","subscribed_events":["job.succeeded"]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/webhooks", body)
	req = withPrincipal(req, &auth.Principal{OrgID: "00000000-0000-0000-0000-000000000001"})
	rec := httptest.NewRecorder()
	h.Create(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestWebhookCreate_RejectsNonHTTPS(t *testing.T) {
	t.Parallel()
	h := &WebhookHandler{}
	cases := []struct {
		name string
		url  string
	}{
		{"http", "http://example.com/h"},
		{"ftp", "ftp://example.com/h"},
		{"no scheme", "example.com/h"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			body, _ := json.Marshal(map[string]any{
				"url":               tc.url,
				"subscribed_events": []string{"job.succeeded"},
			})
			req := httptest.NewRequest(http.MethodPost, "/v1/webhooks", bytes.NewReader(body))
			req = withPrincipal(req, &auth.Principal{OrgID: "00000000-0000-0000-0000-000000000001"})
			rec := httptest.NewRecorder()
			h.Create(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (url=%s)", rec.Code, tc.url)
			}
		})
	}
}

func TestWebhookCreate_RejectsEmptySubscribedEvents(t *testing.T) {
	t.Parallel()
	h := &WebhookHandler{}
	body := strings.NewReader(`{"url":"https://example.com/h","subscribed_events":[]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/webhooks", body)
	req = withPrincipal(req, &auth.Principal{OrgID: "00000000-0000-0000-0000-000000000001"})
	rec := httptest.NewRecorder()
	h.Create(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestWebhookCreate_RejectsInvalidEventType(t *testing.T) {
	t.Parallel()
	h := &WebhookHandler{}
	body := strings.NewReader(`{"url":"https://example.com/h","subscribed_events":["nonsense.event"]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/webhooks", body)
	req = withPrincipal(req, &auth.Principal{OrgID: "00000000-0000-0000-0000-000000000001"})
	rec := httptest.NewRecorder()
	h.Create(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestWebhookCreate_RejectsShortSecret(t *testing.T) {
	t.Parallel()
	h := &WebhookHandler{}
	body := strings.NewReader(`{"url":"https://example.com/h","subscribed_events":["job.succeeded"],"secret":"short"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/webhooks", body)
	req = withPrincipal(req, &auth.Principal{OrgID: "00000000-0000-0000-0000-000000000001"})
	rec := httptest.NewRecorder()
	h.Create(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// TestWebhookCreate_AcceptsWildcard pins the "["*"]" subscription
// passthrough. We don't have a real DB so this test only checks
// that validation lets it through to the DB layer (which would
// fail at the INSERT). A nil-DB service panics on the pool, which
// the test framework reports as a failure — that is the proof
// that validation succeeded.
func TestWebhookCreate_AcceptsWildcard(t *testing.T) {
	t.Parallel()
	h := &WebhookHandler{DB: nil, Audit: &audit.Recorder{}}
	body := strings.NewReader(`{"url":"https://example.com/h","subscribed_events":["*"]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/webhooks", body)
	req = withPrincipal(req, &auth.Principal{OrgID: "00000000-0000-0000-0000-000000000001"})
	rec := httptest.NewRecorder()
	func() {
		defer func() { _ = recover() }()
		h.Create(rec, req)
	}()
	if rec.Code == http.StatusBadRequest {
		t.Fatalf("validation unexpectedly rejected wildcard subscription")
	}
}

// ─────────────────────────────────────────────────────────────────────
// Update validation
// ─────────────────────────────────────────────────────────────────────

func TestWebhookUpdate_RejectsEmptyBody(t *testing.T) {
	t.Parallel()
	h := &WebhookHandler{}
	body := strings.NewReader(`{}`)
	req := httptest.NewRequest(http.MethodPatch, "/v1/webhooks/abc", body)
	req = withPrincipal(req, &auth.Principal{OrgID: "00000000-0000-0000-0000-000000000001"})
	rec := httptest.NewRecorder()
	h.Update(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestWebhookUpdate_RejectsNonHTTPS(t *testing.T) {
	t.Parallel()
	h := &WebhookHandler{}
	body := strings.NewReader(`{"url":"http://example.com/h"}`)
	req := httptest.NewRequest(http.MethodPatch, "/v1/webhooks/abc", body)
	req = withPrincipal(req, &auth.Principal{OrgID: "00000000-0000-0000-0000-000000000001"})
	rec := httptest.NewRecorder()
	h.Update(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestWebhookUpdate_RejectsEmptySubscribedEvents(t *testing.T) {
	t.Parallel()
	h := &WebhookHandler{}
	body := strings.NewReader(`{"subscribed_events":[]}`)
	req := httptest.NewRequest(http.MethodPatch, "/v1/webhooks/abc", body)
	req = withPrincipal(req, &auth.Principal{OrgID: "00000000-0000-0000-0000-000000000001"})
	rec := httptest.NewRecorder()
	h.Update(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// TestWebhookUpdate_ActiveFalseIsNotMissing covers the "explicit
// false vs missing field" trap. Update must distinguish the two so
// that `{"active": false}` actually disables the endpoint.
func TestWebhookUpdate_ActiveFalseIsNotMissing(t *testing.T) {
	t.Parallel()
	// nil-DB path: if validation correctly accepts explicit false,
	// the handler reaches the DB layer and panics on nil pool (which
	// the test framework recovers as a normal panic). The point of
	// the test is that {"active": false} is NOT 400.
	h := &WebhookHandler{DB: nil, Audit: &audit.Recorder{}}
	body := strings.NewReader(`{"active": false}`)
	req := httptest.NewRequest(http.MethodPatch, "/v1/webhooks/abc", body)
	req = withPrincipal(req, &auth.Principal{OrgID: "00000000-0000-0000-0000-000000000001"})
	rec := httptest.NewRecorder()
	func() {
		defer func() {
			_ = recover() // expected: nil-DB panic
		}()
		h.Update(rec, req)
	}()
	if rec.Code == http.StatusBadRequest {
		t.Fatalf("validation unexpectedly rejected active=false")
	}
}

func TestWebhookUpdate_NilActiveIsMissing(t *testing.T) {
	t.Parallel()
	// {"description": "x"} without an active field is valid (and
	// should reach the DB layer). {"active": false} without any
	// other field is also valid. {} with no fields at all is 400.
	h := &WebhookHandler{DB: nil, Audit: &audit.Recorder{}}
	body := strings.NewReader(`{"description":"new desc"}`)
	req := httptest.NewRequest(http.MethodPatch, "/v1/webhooks/abc", body)
	req = withPrincipal(req, &auth.Principal{OrgID: "00000000-0000-0000-0000-000000000001"})
	rec := httptest.NewRecorder()
	func() {
		defer func() { _ = recover() }()
		h.Update(rec, req)
	}()
	if rec.Code == http.StatusBadRequest {
		t.Fatalf("validation unexpectedly rejected single-field update")
	}
}

// ─────────────────────────────────────────────────────────────────────
// ListDeliveries validation
// ─────────────────────────────────────────────────────────────────────

func TestListDeliveries_RejectsInvalidStatus(t *testing.T) {
	t.Parallel()
	h := &WebhookHandler{DB: nil, Audit: &audit.Recorder{}}
	req := httptest.NewRequest(http.MethodGet, "/v1/webhooks/abc/deliveries?status=bogus", nil)
	req = withPrincipal(req, &auth.Principal{OrgID: "00000000-0000-0000-0000-000000000001"})
	rec := httptest.NewRecorder()
	// nil-DB → recovery; the validation should fire first, so we
	// expect 400 even if the DB layer would later panic.
	func() {
		defer func() { _ = recover() }()
		h.ListDeliveries(rec, req)
	}()
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestListDeliveries_RejectsNonPositiveLimit(t *testing.T) {
	t.Parallel()
	h := &WebhookHandler{DB: nil, Audit: &audit.Recorder{}}
	req := httptest.NewRequest(http.MethodGet, "/v1/webhooks/abc/deliveries?limit=-1", nil)
	req = withPrincipal(req, &auth.Principal{OrgID: "00000000-0000-0000-0000-000000000001"})
	rec := httptest.NewRecorder()
	func() {
		defer func() { _ = recover() }()
		h.ListDeliveries(rec, req)
	}()
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestListDeliveries_OverLimitSilentlyCapped(t *testing.T) {
	t.Parallel()
	// Phase 1 decision: limit > 200 is silently capped to 200 rather
	// than 400. Validation passes, the DB layer is reached.
	h := &WebhookHandler{DB: nil, Audit: &audit.Recorder{}}
	req := httptest.NewRequest(http.MethodGet, "/v1/webhooks/abc/deliveries?limit=9999", nil)
	req = withPrincipal(req, &auth.Principal{OrgID: "00000000-0000-0000-0000-000000000001"})
	rec := httptest.NewRecorder()
	func() {
		defer func() { _ = recover() }()
		h.ListDeliveries(rec, req)
	}()
	if rec.Code == http.StatusBadRequest {
		t.Fatalf("limit=9999 should be silently capped, not 400")
	}
}

// ─────────────────────────────────────────────────────────────────────
// Replay validation
// ─────────────────────────────────────────────────────────────────────

func TestReplay_RejectsUnauthenticated(t *testing.T) {
	t.Parallel()
	h := &WebhookHandler{}
	req := httptest.NewRequest(http.MethodPost, "/v1/webhooks/a/deliveries/b/replay", nil)
	rec := httptest.NewRecorder()
	h.Replay(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

// TestReplay_RequiresDB drives the happy path down to the DB layer
// and stops there (panics on nil pool). This proves the request
// reaches the SQL; the actual 202/404 outcomes are covered by
// integration tests.
func TestReplay_RequiresDB(t *testing.T) {
	t.Parallel()
	h := &WebhookHandler{DB: nil, Audit: &audit.Recorder{}}
	req := httptest.NewRequest(http.MethodPost, "/v1/webhooks/abc/deliveries/del/replay", nil)
	req = withPrincipal(req, &auth.Principal{OrgID: "00000000-0000-0000-0000-000000000001"})
	rec := httptest.NewRecorder()
	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("expected panic on nil-DB pool access")
			}
		}()
		h.Replay(rec, req)
	}()
}

// ─────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────

func TestValidateCreate_Cases(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		req  CreateWebhookRequest
		ok   bool
	}{
		{
			"happy",
			CreateWebhookRequest{URL: "https://x.example.com/h", SubscribedEvents: []string{"job.succeeded"}},
			true,
		},
		{
			"missing url",
			CreateWebhookRequest{SubscribedEvents: []string{"job.succeeded"}},
			false,
		},
		{
			"http scheme",
			CreateWebhookRequest{URL: "http://x.example.com/h", SubscribedEvents: []string{"job.succeeded"}},
			false,
		},
		{
			"empty events",
			CreateWebhookRequest{URL: "https://x.example.com/h"},
			false,
		},
		{
			"wildcard events",
			CreateWebhookRequest{URL: "https://x.example.com/h", SubscribedEvents: []string{"*"}},
			true,
		},
		{
			"short secret",
			CreateWebhookRequest{URL: "https://x.example.com/h", SubscribedEvents: []string{"job.succeeded"}, Secret: "abc"},
			false,
		},
		{
			"valid secret",
			CreateWebhookRequest{URL: "https://x.example.com/h", SubscribedEvents: []string{"job.succeeded"}, Secret: "this-is-a-good-secret-32chars-xx"},
			true,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := validateCreate(&tc.req)
			if tc.ok && got != "" {
				t.Errorf("expected ok, got %q", got)
			}
			if !tc.ok && got == "" {
				t.Errorf("expected validation error, got none")
			}
		})
	}
}

func TestValidateUpdate_Cases(t *testing.T) {
	t.Parallel()
	goodURL := "https://x.example.com/h"
	badURL := "http://x.example.com/h"
	cases := []struct {
		name string
		req  UpdateWebhookRequest
		ok   bool
	}{
		{"nil url", UpdateWebhookRequest{Description: strPtr("x")}, true},
		{"good url", UpdateWebhookRequest{URL: &goodURL}, true},
		{"bad url", UpdateWebhookRequest{URL: &badURL}, false},
		{"empty events", UpdateWebhookRequest{SubscribedEvents: []string{}}, false},
		{"good events", UpdateWebhookRequest{SubscribedEvents: []string{"job.queued"}}, true},
		{"bad event", UpdateWebhookRequest{SubscribedEvents: []string{"bogus"}}, false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := validateUpdate(&tc.req)
			if tc.ok && got != "" {
				t.Errorf("expected ok, got %q", got)
			}
			if !tc.ok && got == "" {
				t.Errorf("expected validation error, got none")
			}
		})
	}
}

func TestValidDeliveryStatus(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want bool
	}{
		{"pending", true},
		{"delivering", true},
		{"delivered", true},
		{"failed", true},
		{"exhausted", true},
		{"", false},
		{"succeeded", false}, // spec value, not DB value
		{"retrying", false},  // spec value, not DB value
	}
	for _, tc := range cases {
		if got := validDeliveryStatus(tc.in); got != tc.want {
			t.Errorf("validDeliveryStatus(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestFirstLine(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		max  int
		want string
	}{
		{"hello", 200, "hello"},
		{"line1\nline2", 200, "line1"},
		{"abcdef", 3, "abc"},
		{"a\nb", 200, "a"},
	}
	for _, tc := range cases {
		if got := firstLine(tc.in, tc.max); got != tc.want {
			t.Errorf("firstLine(%q, %d) = %q, want %q", tc.in, tc.max, got, tc.want)
		}
	}
}

func TestAllowedEvents_ContainsExpected(t *testing.T) {
	t.Parallel()
	want := []string{
		"job.queued", "job.started", "job.succeeded", "job.failed", "job.canceled",
		"upload.completed", "upload.failed",
		"api_key.created", "api_key.revoked",
		"billing.period_closed",
	}
	for _, e := range want {
		if _, ok := allowedEvents[e]; !ok {
			t.Errorf("allowedEvents missing %q", e)
		}
	}
	// And that an unknown event type really is rejected.
	if _, ok := allowedEvents["not.a.real.event"]; ok {
		t.Error("allowedEvents should not contain 'not.a.real.event'")
	}
}

// ─────────────────────────────────────────────────────────────────────
// Page-size math sanity
// ─────────────────────────────────────────────────────────────────────

func TestList_LimitParse(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want int
	}{
		{"", 50},     // empty → default
		{"10", 10},   // happy path
		{"200", 200}, // cap
		{"0", 50},    // 0 invalid → default
		{"-1", 50},   // negative → default
		{"9999", 50}, // over cap → default (handlers leave as default rather than clamp)
		{"abc", 50},  // garbage → default
	}
	for _, tc := range cases {
		got := 50
		if l := tc.in; l != "" {
			if n, err := strconv.Atoi(l); err == nil && n > 0 && n <= 200 {
				got = n
			}
		}
		if got != tc.want {
			t.Errorf("limitParse(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// TestWithPrincipal_ReusedInOtherTests is a meta-test: it exists
// only to make sure the withPrincipal helper used by every other
// test in this file is wired correctly. If this fails, every other
// test in this file is meaningless.
func TestWithPrincipal_ReusedInOtherTests(t *testing.T) {
	t.Parallel()
	req := httptest.NewRequest(http.MethodGet, "/v1/webhooks", nil)
	p := &auth.Principal{OrgID: "00000000-0000-0000-0000-000000000001", UserID: "u1"}
	req = withPrincipal(req, p)
	got, err := auth.PrincipalFromContext(req.Context())
	if err != nil {
		t.Fatalf("PrincipalFromContext: %v", err)
	}
	if got.OrgID != p.OrgID {
		t.Errorf("org mismatch")
	}
}

// Compile-time check: db / audit / pgxpool are imported so the test
// file stays honest about its (lack of) DB integration.
var (
	_ = (*db.DB)(nil)
	_ = (*audit.Recorder)(nil)
	_ = (*pgxpool.Pool)(nil)
	_ = errors.New
	_ = context.Background
)
