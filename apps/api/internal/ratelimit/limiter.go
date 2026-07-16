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
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

// slidingWindowScript performs the whole sliding-window check atomically
// in Redis, so concurrent requests cannot interleave between prune / add
// / count and slip past the cap (the previous pipeline was not atomic).
// It prunes expired members, adds the current request, counts the window,
// refreshes the TTL, and returns {count, oldestScoreMs} so the caller can
// compute Retry-After without a second round-trip.
//
// KEYS[1] = sorted-set key
// ARGV[1] = now (ms), ARGV[2] = windowStart (ms), ARGV[3] = ttl (ms),
// ARGV[4] = unique member
var slidingWindowScript = redis.NewScript(`
local key = KEYS[1]
local now = tonumber(ARGV[1])
local windowStart = tonumber(ARGV[2])
local ttl = tonumber(ARGV[3])
local member = ARGV[4]
redis.call('ZREMRANGEBYSCORE', key, '-inf', windowStart)
redis.call('ZADD', key, now, member)
local count = redis.call('ZCARD', key)
redis.call('PEXPIRE', key, ttl)
local oldest = redis.call('ZRANGE', key, 0, 0, 'WITHSCORES')
local oldestScore = now
if oldest[2] then oldestScore = tonumber(oldest[2]) end
return {count, oldestScore}
`)

// memberSeq guarantees unique sorted-set members even when two requests
// land in the same millisecond, so distinct requests are always counted
// distinctly (identical members would collapse under ZADD).
var memberSeq atomic.Uint64

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

// Allow checks whether a request from key is allowed under plan's quota.
// The returned retryAfter is non-zero only when allowed=false.
//
// The prune / add / count / expire steps run as a single atomic Lua
// script (see slidingWindowScript), so concurrent requests cannot
// interleave and slip past the cap. Scores are milliseconds (not
// nanoseconds) to stay within float64's exact-integer range. We add the
// current request *before* checking the count; the worst case is one
// extra request past the limit, which is tolerable for a soft cap.
func (l *Limiter) Allow(ctx context.Context, key, plan string) (bool, time.Duration, error) {
	if l == nil || l.rdb == nil {
		return true, 0, nil
	}
	limit := l.LimitFor(plan)
	if limit <= 0 {
		return true, 0, nil
	}

	now := time.Now()
	nowMs := now.UnixMilli()
	windowMs := Window.Milliseconds()
	windowStartMs := nowMs - windowMs
	// Unique member: ms timestamp + a process-local sequence so two
	// requests in the same millisecond are still counted separately.
	member := strconv.FormatInt(nowMs, 10) + "-" + strconv.FormatUint(memberSeq.Add(1), 10)
	ttlMs := windowMs + 1000

	res, err := slidingWindowScript.Run(ctx, l.rdb, []string{redisKey(key)},
		nowMs, windowStartMs, ttlMs, member).Result()
	if err != nil {
		return false, 0, fmt.Errorf("ratelimit.eval: %w", err)
	}
	vals, ok := res.([]any)
	if !ok || len(vals) < 2 {
		return false, 0, fmt.Errorf("ratelimit.eval: unexpected result %v", res)
	}
	count, _ := vals[0].(int64)
	oldestMs, _ := vals[1].(int64)

	if count <= int64(limit) {
		return true, 0, nil
	}

	// Over the cap. Retry once the oldest in-window entry falls off.
	retry := time.Duration(oldestMs+windowMs-nowMs) * time.Millisecond
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
