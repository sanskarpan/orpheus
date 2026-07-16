package ratelimit

import (
	"context"
	"os"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// TestAllow_AtomicUnderConcurrency proves the sliding window enforces the
// cap EXACTLY under true concurrency: with a limit of FreeLimit, firing
// many simultaneous requests for one key must allow exactly FreeLimit and
// deny the rest. The previous non-atomic pipeline could let extras
// through because prune/add/count could interleave.
func TestAllow_AtomicUnderConcurrency(t *testing.T) {
	url := os.Getenv("ORPHEUS_TEST_REDIS_URL")
	if url == "" {
		t.Skip("ORPHEUS_TEST_REDIS_URL not set")
	}
	opt, err := redis.ParseURL(url)
	if err != nil {
		t.Fatalf("ParseURL: %v", err)
	}
	rdb := redis.NewClient(opt)
	t.Cleanup(func() { _ = rdb.Close() })
	ctx := context.Background()
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Skipf("Redis not reachable: %v", err)
	}

	lim := New(rdb) // FreeLimit = 60
	key := "atomic-" + uuid.NewString()
	t.Cleanup(func() { _ = rdb.Del(context.Background(), redisKey(key)).Err() })

	const n = 200
	var allowed atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			ok, _, err := lim.Allow(ctx, key, "free")
			if err != nil {
				t.Errorf("Allow: %v", err)
				return
			}
			if ok {
				allowed.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()

	if got := allowed.Load(); got != int64(FreeLimit) {
		t.Fatalf("allowed %d of %d concurrent requests, want exactly %d (atomicity broken)", got, n, FreeLimit)
	}
}
