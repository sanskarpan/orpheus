package ratelimit

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/orpheus/api/internal/auth"
)

// Middleware is the chi-compatible HTTP wrapper around [Limiter]. It
// is safe to share across requests: the underlying limiter holds no
// per-request state.
type Middleware struct {
	Limiter *Limiter
	Logger  *slog.Logger
}

// NewMiddleware constructs a Middleware. logger may be nil (defaults to
// [slog.Default]). The function is named NewMiddleware (not New) to
// avoid collision with the limiter's constructor in the same package.
func NewMiddleware(limiter *Limiter, logger *slog.Logger) *Middleware {
	if logger == nil {
		logger = slog.Default()
	}
	return &Middleware{Limiter: limiter, Logger: logger}
}

// Handler returns the http.Handler middleware. It must be installed
// after the auth middleware so the principal is in context.
//
// Behaviour:
//
//   - No principal: pass through. Auth will respond.
//   - Redis error: fail open. The request is allowed and the error is
//     logged at WARN. Dropping traffic because the limiter is down is
//     not acceptable.
//   - Allowed: pass through with X-RateLimit-Limit + Remaining set.
//   - Denied: respond 429 with Retry-After + X-RateLimit-* headers and
//     a problem+json body.
func (m *Middleware) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, err := auth.PrincipalFromContext(r.Context())
		if err != nil || p == nil {
			next.ServeHTTP(w, r)
			return
		}

		key, plan := bucketFor(p, r.Context())
		allowed, retry, err := m.Limiter.Allow(r.Context(), key, plan)
		if err != nil {
			// Fail open. A working limiter is not load-bearing for
			// correctness, but a broken one shouldn't black-hole
			// production traffic.
			m.Logger.Warn("ratelimit.allow_failed",
				"err", err,
				"key", key,
				"plan", plan,
			)
			next.ServeHTTP(w, r)
			return
		}

		limit := m.Limiter.LimitFor(plan)
		w.Header().Set("X-RateLimit-Limit", strconv.Itoa(limit))
		// Remaining is computed as best-effort: at the cap we can
		// only say 0. A precise remaining-count would require
		// another Redis round-trip; not worth the latency hit on
		// every request.
		w.Header().Set("X-RateLimit-Remaining", "0")

		if !allowed {
			seconds := int(retry.Seconds())
			if seconds < 1 {
				seconds = 1
			}
			w.Header().Set("Retry-After", strconv.Itoa(seconds))
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(retryDuration(retry), 10))
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(limitProblemBody(seconds)))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// bucketFor picks the Redis key + plan. API-key traffic uses a
// per-key bucket so a noisy script can't drown out human users in the
// same org; everything else (JWT) goes to the org bucket. The plan
// defaults to "free" — the auth package does not currently carry the
// plan on the Principal, so a future "plan resolver" middleware
// (e.g. a Redis-cached lookup of the org's billing plan) can attach
// it via [WithPlan] and the limiter will read it from ctx.
func bucketFor(p *auth.Principal, ctx context.Context) (key, plan string) {
	plan = "free"
	if v := PlanFromContext(ctx); v != "" {
		plan = v
	}
	if p == nil {
		return "", plan
	}
	if p.APIKeyID != "" {
		return "apikey:" + p.APIKeyID, plan
	}
	return "org:" + p.OrgID, plan
}

// retryDuration converts a duration to the seconds-until-reset integer
// the X-RateLimit-Reset header wants. The header is documented in
// draft-ietf-httpapi-ratelimit-headers and conventionally an epoch
// second; we emit a delta from now because callers can compute the
// absolute value trivially.
func retryDuration(d time.Duration) int64 {
	secs := int64(d.Seconds())
	if secs < 1 {
		return 1
	}
	return secs
}

// limitProblemBody is the JSON body for 429 responses. We hand-format
// it to avoid pulling encoding/json into the hot path.
func limitProblemBody(retrySeconds int) string {
	return `{"type":"https://docs.orpheus.dev/errors/rate-limited","title":"Too Many Requests","status":429,"detail":"retry after ` +
		strconv.Itoa(retrySeconds) + ` seconds"}`
}

// planContextKey is the unexported context key used by [WithPlan] and
// [PlanFromContext]. Future plan-resolver middleware can call
// [WithPlan] to attach the org's billing plan; the rate limiter then
// reads it via [PlanFromContext] and skips a DB round-trip on every
// request.
type planContextKey struct{}

// WithPlan returns a new context carrying the org's billing plan. The
// rate limiter reads it from [PlanFromContext] inside [bucketFor].
// This indirection is what lets the limiter stay independent of the
// orgs table — pricing data is fetched once at request start and
// cached for the duration of the request, not on every quota check.
func WithPlan(ctx context.Context, plan string) context.Context {
	return context.WithValue(ctx, planContextKey{}, plan)
}

// PlanFromContext returns the plan attached to ctx by [WithPlan], or
// "" if none is set. The caller should treat "" as "use the default
// plan".
func PlanFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(planContextKey{}).(string); ok {
		return v
	}
	return ""
}
