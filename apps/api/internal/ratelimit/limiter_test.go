package ratelimit

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/redis/go-redis/v9"

	"github.com/orpheus/api/internal/auth"
)

// TestAllowDisabled confirms the nil-Redis path: a Limiter constructed
// with rdb=nil allows every request, which is the "feature off" mode
// we want in dev / in tests that don't stand up Redis.
func TestAllowDisabled(t *testing.T) {
	l := New(nil)
	ok, retry, err := l.Allow(context.Background(), "user:x", "free")
	if err != nil {
		t.Fatalf("Allow: %v", err)
	}
	if !ok {
		t.Error("Allow returned false; want true (limiter disabled)")
	}
	if retry != 0 {
		t.Errorf("retry = %v, want 0", retry)
	}
}

// TestLimitFor pins the plan -> quota mapping. Pricing changes should
// be a one-line edit in limiter.go plus a test update; this catches
// accidental regressions like swapping the free/paid numbers.
func TestLimitFor(t *testing.T) {
	l := New(nil)
	cases := map[string]int{
		"":           FreeLimit,
		"free":       FreeLimit,
		"paid":       PaidLimit,
		"enterprise": EnterpriseLimit,
		"unknown":    FreeLimit,
	}
	for plan, want := range cases {
		if got := l.LimitFor(plan); got != want {
			t.Errorf("LimitFor(%q) = %d, want %d", plan, got, want)
		}
	}
}

// TestBucketFor covers the principal -> (key, plan) projection. The
// shape is part of the public contract: API-key callers must be
// bucketised separately from human users.
func TestBucketFor(t *testing.T) {
	cases := []struct {
		name     string
		p        *auth.Principal
		wantKey  string
		wantPlan string
	}{
		{"api key", &auth.Principal{APIKeyID: "ak-1", OrgID: "o-1"}, "apikey:ak-1", "free"},
		{"user", &auth.Principal{UserID: "u-1", OrgID: "o-1"}, "org:o-1", "free"},
		{"nil principal", nil, "", "free"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			key, plan := bucketFor(tc.p, context.Background())
			if key != tc.wantKey || plan != tc.wantPlan {
				t.Errorf("bucketFor(%+v) = (%q, %q), want (%q, %q)",
					tc.p, key, plan, tc.wantKey, tc.wantPlan)
			}
		})
	}
}

// TestBucketForHonoursPlanContext confirms a future plan-resolver
// middleware can override the default "free" plan by attaching a value
// to the request context via [WithPlan].
func TestBucketForHonoursPlanContext(t *testing.T) {
	ctx := WithPlan(context.Background(), "paid")
	p := &auth.Principal{UserID: "u-1", OrgID: "o-1"}
	key, plan := bucketFor(p, ctx)
	if key != "org:o-1" {
		t.Errorf("key = %q, want org:o-1", key)
	}
	if plan != "paid" {
		t.Errorf("plan = %q, want paid", plan)
	}
}

// TestHandlerNoPrincipal: when the principal isn't in the context, the
// middleware is a passthrough. Auth will respond with 401; we just
// shouldn't be in the way.
func TestHandlerNoPrincipal(t *testing.T) {
	m := NewMiddleware(New(nil), slog.New(slog.NewTextHandler(io.Discard, nil)))
	called := false
	h := m.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/v1/uploads", nil)
	h.ServeHTTP(httptest.NewRecorder(), req)
	if !called {
		t.Fatal("downstream handler not invoked when principal missing")
	}
}

// TestHandlerDisabledLimiter: with rdb=nil, the limiter allows
// everything and the middleware sets X-RateLimit-Limit but no
// Retry-After. This is the dev-mode smoke test.
func TestHandlerDisabledLimiter(t *testing.T) {
	lim := New(nil)
	m := NewMiddleware(lim, slog.New(slog.NewTextHandler(io.Discard, nil)))
	called := false
	h := m.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
	}))
	ctx := auth.WithPrincipal(context.Background(),
		&auth.Principal{UserID: "u-1", OrgID: "o-1"})

	req := httptest.NewRequest(http.MethodGet, "/v1/uploads", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if !called {
		t.Fatal("downstream handler not invoked")
	}
	if got := rec.Header().Get("X-RateLimit-Limit"); got != strconvI(FreeLimit) {
		t.Errorf("X-RateLimit-Limit = %q, want %q", got, strconvI(FreeLimit))
	}
	if got := rec.Header().Get("Retry-After"); got != "" {
		t.Errorf("Retry-After set on allowed request: %q", got)
	}
}

// TestPlanContextRoundTrip is a small smoke test for the plan context
// helpers. It's redundant with TestBucketForHonoursPlanContext but
// pins the public API in place.
func TestPlanContextRoundTrip(t *testing.T) {
	if got := PlanFromContext(context.Background()); got != "" {
		t.Errorf("empty context plan = %q, want \"\"", got)
	}
	ctx := WithPlan(context.Background(), "enterprise")
	if got := PlanFromContext(ctx); got != "enterprise" {
		t.Errorf("WithPlan/PlanFromContext = %q, want enterprise", got)
	}
}

// TestHandlerWithRedis exercises the live-Redis path. It is skipped
// when ORPHEUS_TEST_REDIS_URL is unset; the unit tests above already
// cover the allow/deny branches. The intent is to catch wiring
// regressions in the ZADD / ZREMRANGEBYSCORE pipeline.
func TestHandlerWithRedis(t *testing.T) {
	if testing.Short() {
		t.Skip("requires ORPHEUS_TEST_REDIS_URL; skipped in -short mode")
	}
	addr := os.Getenv("ORPHEUS_TEST_REDIS_URL")
	if addr == "" {
		t.Skip("ORPHEUS_TEST_REDIS_URL not set; skipping live-Redis test")
	}

	rdb := redis.NewClient(&redis.Options{Addr: strings.TrimPrefix(addr, "redis://")})
	t.Cleanup(func() { _ = rdb.Close() })

	ctx := context.Background()
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Skipf("Redis not reachable: %v", err)
	}

	lim := New(rdb)
	m := NewMiddleware(lim, slog.New(slog.NewTextHandler(io.Discard, nil)))

	ctx = auth.WithPrincipal(ctx,
		&auth.Principal{UserID: "u-1", OrgID: "o-rl-test"})
	// Fire N+1 requests in a tight loop. The (N+1)th should 429.
	for i := 0; i < FreeLimit; i++ {
		req := httptest.NewRequest(http.MethodGet, "/v1/uploads", nil).WithContext(ctx)
		rec := httptest.NewRecorder()
		m.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		})).ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d returned %d, want 200", i, rec.Code)
		}
	}
	// The (N+1)th request should be over the cap.
	req := httptest.NewRequest(http.MethodGet, "/v1/uploads", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	m.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("downstream handler invoked on rate-limited request")
	})).ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got == "" {
		t.Error("Retry-After header missing on 429")
	}
	if got := rec.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Errorf("Content-Type = %q, want application/problem+json", got)
	}
}

// strconvI is a tiny helper so the test above doesn't have to import
// strconv directly (keeps the imports list tight).
func strconvI(n int) string {
	const digits = "0123456789"
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = digits[n%10]
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// TestMiddleware_FailClosedOnRedisError verifies the configurable
// behaviour when the limiter backend errors: FailClosed=true → 503,
// FailClosed=false (default) → pass through. A Redis client pointed at a
// closed port makes Limiter.Allow return an error without needing a live
// Redis.
func TestMiddleware_FailClosedOnRedisError(t *testing.T) {
	deadRDB := redis.NewClient(&redis.Options{
		Addr:        "127.0.0.1:1", // nothing listens here → dial error
		DialTimeout: 200_000_000,   // 200ms
	})
	t.Cleanup(func() { _ = deadRDB.Close() })
	lim := New(deadRDB)

	nextCalled := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextCalled = true
		w.WriteHeader(http.StatusOK)
	})

	newReq := func() *http.Request {
		req := httptest.NewRequest(http.MethodGet, "/v1/jobs", nil)
		return req.WithContext(auth.WithPrincipal(req.Context(), &auth.Principal{OrgID: "org-1"}))
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Fail closed → 503, handler not reached.
	nextCalled = false
	mClosed := NewMiddleware(lim, logger)
	mClosed.FailClosed = true
	recC := httptest.NewRecorder()
	mClosed.Handler(next).ServeHTTP(recC, newReq())
	if recC.Code != http.StatusServiceUnavailable {
		t.Errorf("FailClosed: status = %d, want 503", recC.Code)
	}
	if nextCalled {
		t.Error("FailClosed: handler should not be reached")
	}

	// Fail open (default) → request passes through.
	nextCalled = false
	mOpen := NewMiddleware(lim, logger)
	recO := httptest.NewRecorder()
	mOpen.Handler(next).ServeHTTP(recO, newReq())
	if !nextCalled || recO.Code != http.StatusOK {
		t.Errorf("FailOpen: code=%d nextCalled=%v, want 200/true", recO.Code, nextCalled)
	}
}
