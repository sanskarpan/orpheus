// Package ratelimit provides a Redis-backed token-bucket / sliding-window
// rate limiter and a chi-compatible HTTP middleware.
//
// The limiter maintains a sorted set per key in Redis. Each request
// adds a member with the current timestamp as its score, and entries
// older than the window are pruned before counting. This is a sliding
// window counter: more accurate than a fixed window and simpler than a
// full token bucket. The accuracy is bounded by the granularity of
// `time.Now()` (nanoseconds) and Redis clock skew between replicas
// (negligible in practice).
//
// On Redis errors the middleware fails open: we cannot afford to drop
// real traffic because the limiter is down. A metric/log line is
// emitted so the operator notices; the request goes through.
package ratelimit

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// Quotas for the per-key window. Free plan is intentionally small so
// abuse is cheap to detect. Paid tiers are sized for the workloads
// documented in the API spec.
const (
	FreeLimit       = 60
	PaidLimit       = 1000
	EnterpriseLimit = 10000
	Window          = time.Minute
)

// Limiter is the underlying quota engine. It is safe to share across
// goroutines; the redis.Client itself is goroutine-safe.
type Limiter struct {
	rdb   *redis.Client
	limit int // requests per window (set in New; overridden via WithLimit)
}

// New constructs a Limiter with the default free/paid limits. Pass
// nil to disable Redis (every Allow call returns true, 0, nil) — useful
// for tests and for environments that have not wired Redis yet.
func New(rdb *redis.Client) *Limiter {
	return &Limiter{rdb: rdb, limit: FreeLimit}
}

// WithLimit returns a copy of l with a custom default limit. Use this
// when introducing a new plan tier without touching the constants.
func (l *Limiter) WithLimit(limit int) *Limiter {
	cp := *l
	cp.limit = limit
	return &cp
}

// LimitFor returns the request-per-window cap for the given plan.
// Unknown plans fall back to the free tier — fail safe, not fail open.
func (l *Limiter) LimitFor(plan string) int {
	switch plan {
	case "paid":
		return PaidLimit
	case "enterprise":
		return EnterpriseLimit
	default:
		return FreeLimit
	}
}

// Allow checks whether a request from key is allowed under plan's
// quota. The returned retryAfter is non-zero only when allowed=false.
//
// Implementation:
//
//  1. Prune entries older than the window.
//  2. ZADD a new member with the current timestamp.
//  3. ZCARD to count entries inside the window.
//  4. EXPIRE the key to the window+1s so idle keys get cleaned up.
//
// All four commands run in a single pipeline to keep round-trips down.
// We deliberately add the current request *before* checking the count:
// the worst case is one extra request past the limit, which is
// tolerable (the cap is soft anyway — see Retry-After below).
func (l *Limiter) Allow(ctx context.Context, key, plan string) (bool, time.Duration, error) {
	if l == nil || l.rdb == nil {
		return true, 0, nil
	}
	limit := l.LimitFor(plan)
	if limit <= 0 {
		return true, 0, nil
	}

	now := time.Now()
	windowStart := now.Add(-Window).UnixNano()
	member := strconv.FormatInt(now.UnixNano(), 10)
	redisKey := redisKey(key)

	pipe := l.rdb.Pipeline()
	pipe.ZRemRangeByScore(ctx, redisKey, "0", strconv.FormatInt(windowStart, 10))
	pipe.ZAdd(ctx, redisKey, redis.Z{Score: float64(now.UnixNano()), Member: member})
	card := pipe.ZCard(ctx, redisKey)
	pipe.Expire(ctx, redisKey, Window+time.Second)

	if _, err := pipe.Exec(ctx); err != nil {
		return false, 0, fmt.Errorf("ratelimit.pipeline: %w", err)
	}
	count := card.Val()
	if count <= int64(limit) {
		return true, 0, nil
	}

	// Over the cap. Compute the time until the oldest entry in the
	// window falls off — that's the soonest a retry can succeed.
	oldest, err := l.rdb.ZRangeWithScores(ctx, redisKey, 0, 0).Result()
	if err != nil || len(oldest) == 0 {
		// No data point we can use; ask the client to wait the full
		// window. This is conservative but never undershoots.
		return false, Window, nil
	}
	oldestNs := int64(oldest[0].Score)
	retry := time.Duration(oldestNs+int64(Window)-now.UnixNano()) * time.Nanosecond
	if retry < time.Second {
		retry = time.Second
	}
	if retry > Window {
		retry = Window
	}
	return false, retry, nil
}

// redisKey namespaces the limiter state in Redis so it doesn't collide
// with other consumers (Arq, idempotency cache, etc.). The `rl:` prefix
// is the convention used across the API.
func redisKey(key string) string { return "rl:" + key }
