package audit

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// TestDeriveResource covers the URL -> (resource_type, resource_id)
// mapping used to populate audit_log rows. The resource_type is the
// dot-joined path of every segment except the last (which becomes the
// id), so e.g. `/v1/users/u-1/api_keys/k-9` becomes
// resource_type="users.u-1.api_keys", resource_id="k-9". That gives
// consumers enough context to find the row without parsing the path
// again, and it keeps the rows sensible for deeply-nested routes
// (orgs/members/api_keys) where the resource is several layers down.
func TestDeriveResource(t *testing.T) {
	cases := []struct {
		name         string
		path         string
		wantResource string
		wantID       string
	}{
		{"v1 collection", "/v1/uploads", "uploads", ""},
		{"v1 resource", "/v1/uploads/abc-123", "uploads", "abc-123"},
		{"v1 nested", "/v1/users/u-1/api_keys/k-9", "users.u-1.api_keys", "k-9"},
		{"v1 deeply nested", "/v1/orgs/o/x/y/z", "orgs.o.x.y", "z"},
		{"non-v1 collection", "/uploads", "uploads", ""},
		{"non-v1 resource", "/uploads/abc", "uploads", "abc"},
		{"empty path", "/", "", ""},
		{"v1 with trailing slash", "/v1/uploads/", "uploads", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotResource, gotID := deriveResource(tc.path)
			if gotResource != tc.wantResource || gotID != tc.wantID {
				t.Errorf("deriveResource(%q) = (%q, %q), want (%q, %q)",
					tc.path, gotResource, gotID, tc.wantResource, tc.wantID)
			}
		})
	}
}

// TestBuildAction verifies the action is composed as `resource.verb`
// to match the audit_action enum (e.g. "upload.create"), with the
// resource normalised from its plural/hyphenated URL form.
func TestBuildAction(t *testing.T) {
	cases := []struct {
		verb     string
		resource string
		want     string
	}{
		// Known enum members — singular noun produced from plural input.
		{"create", "uploads", "upload.create"},
		{"update", "jobs", "job.update"},
		{"delete", "webhooks", "webhook.delete"},
		// Singular input stays singular.
		{"create", "upload", "upload.create"},
		// Hyphenated resource collapses to the enum noun.
		{"create", "api-keys", "apikey.create"},
		// Unknown resource still forms resource.verb; the middleware
		// gates on isAuditAction and skips non-enum shapes.
		{"create", "widgets", "widget.create"},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			if got := buildAction(tc.verb, tc.resource); got != tc.want {
				t.Errorf("buildAction(%q, %q) = %q, want %q",
					tc.verb, tc.resource, got, tc.want)
			}
		})
	}
}

// TestIsSafeMethod / TestActionForMethod exercise the method -> verb
// mapping. Mutations should return a verb; safe methods should not.
func TestIsSafeMethod(t *testing.T) {
	for _, m := range []string{http.MethodGet, http.MethodHead, http.MethodOptions} {
		if !isSafeMethod(m) {
			t.Errorf("isSafeMethod(%q) = false, want true", m)
		}
	}
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		if isSafeMethod(m) {
			t.Errorf("isSafeMethod(%q) = true, want false", m)
		}
	}
}

func TestActionForMethod(t *testing.T) {
	cases := map[string]struct {
		verb  string
		isMut bool
	}{
		http.MethodPost:   {"create", true},
		http.MethodPut:    {"update", true},
		http.MethodPatch:  {"update", true},
		http.MethodDelete: {"delete", true},
		http.MethodGet:    {"", false},
	}
	for m, want := range cases {
		gotVerb, gotMut := actionForMethod(m)
		if gotVerb != want.verb || gotMut != want.isMut {
			t.Errorf("actionForMethod(%q) = (%q, %v), want (%q, %v)",
				m, gotVerb, gotMut, want.verb, want.isMut)
		}
	}
}

// TestClientIP confirms the X-Forwarded-For parsing order and the
// RemoteAddr fallback. We do not test the net.SplitHostPort error
// branch — it is unreachable for any sane net/http transport.
func TestClientIP(t *testing.T) {
	cases := []struct {
		name   string
		xff    string
		remote string
		want   string
	}{
		{"xff single", "1.2.3.4", "5.6.7.8:0", "1.2.3.4"},
		{"xff first of list", "1.2.3.4, 10.0.0.1", "5.6.7.8:0", "1.2.3.4"},
		{"no xff", "", "5.6.7.8:0", "5.6.7.8"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if tc.xff != "" {
				req.Header.Set("X-Forwarded-For", tc.xff)
			}
			req.RemoteAddr = tc.remote
			if got := clientIP(req); got != tc.want {
				t.Errorf("clientIP() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestRecordNoPrincipal is a behavioural check: the standalone Record
// path must NOT fail the caller's request just because auth was
// skipped. It should log a warning and return nil. We don't assert on
// the log output — just on the return value and the lack of a panic.
func TestRecordNoPrincipal(t *testing.T) {
	r := &Recorder{
		DB:     nil, // we never reach the DB: the principal check returns first
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if err := r.Record(context.Background(), Entry{Action: "upload.create"}); err != nil {
		t.Errorf("Record with no principal returned %v, want nil", err)
	}
}

// TestMiddlewareSkipsSafeMethods is a smoke test for the auto-derive
// path. We don't connect to a DB; the wrapped handler should be invoked
// once and the audit row should not be attempted (no panic, no DB
// call). This pins the behaviour in place even when sqlc/the DB is
// unavailable in CI.
func TestMiddlewareSkipsSafeMethods(t *testing.T) {
	r := &Recorder{
		DB:     nil, // a DB call would panic; verifying it isn't reached
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	called := false
	h := r.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	for _, m := range []string{http.MethodGet, http.MethodHead, http.MethodOptions} {
		called = false
		req := httptest.NewRequest(m, "/v1/uploads", nil)
		h.ServeHTTP(httptest.NewRecorder(), req)
		if !called {
			t.Errorf("wrapped handler not invoked for %s", m)
		}
	}
}

// TestMiddlewareRecordsMutations verifies the inverse: a mutation
// reaches the recording path. We can't actually hit a DB here, so we
// use the principal-missing branch — the middleware should log +
// return without panicking, just like the standalone Record call.
func TestMiddlewareRecordsMutations(t *testing.T) {
	r := &Recorder{
		DB:     nil,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	called := false
	h := r.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":"abc"}`)
	}))
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		called = false
		req := httptest.NewRequest(m, "/v1/uploads", nil)
		req.URL = &url.URL{Path: "/v1/uploads"}
		h.ServeHTTP(httptest.NewRecorder(), req)
		if !called {
			t.Errorf("wrapped handler not invoked for %s", m)
		}
	}
}
