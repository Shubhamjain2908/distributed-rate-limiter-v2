//go:build integration

// Package redis: integration tests need Docker. Run from repo root:
//
//	go test -tags=integration -race -v ./internal/redis/...
package redis

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	redisv9 "github.com/redis/go-redis/v9"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
)

// Many goroutines fight for the same key with cost=1; Redis serializes the Lua.
// With a fixed clock (no elapsed refill), exactly cap calls may be allowed; the rest must be denied.
func TestIntegration_ConcurrentTokenBucket_RespectsCapacity(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	c, err := tcredis.RunContainer(ctx) // default: docker.io/redis:7
	if err != nil {
		t.Fatalf("start redis: %v", err)
	}
	t.Cleanup(func() {
		if err := c.Terminate(context.Background()); err != nil {
			t.Logf("terminate redis: %v", err)
		}
	})

	uri, err := c.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("ConnectionString: %v", err)
	}
	opts, err := redisv9.ParseURL(uri)
	if err != nil {
		t.Fatalf("ParseURL: %v", err)
	}
	rdb := redisv9.NewClient(opts)

	const capN = 200
	rl, err := NewRedisLimiter(ctx, rdb, "it", capN, 1_000_000.0)
	if err != nil {
		t.Fatalf("NewRedisLimiter: %v", err)
	}
	// One instant: no time-based refill between EVALs; only serial Lua decrements matter.
	frozen := time.Unix(1_700_000_000, 0)
	rl.now = func() time.Time { return frozen }

	const total = 2_000
	var allowed int32
	var denied int32
	var loadErr atomic.Value // error
	var wg sync.WaitGroup
	wg.Add(total)
	for range total {
		go func() {
			defer wg.Done()
			g, err := rl.Allow(ctx, "client-concurrent", 1)
			if err != nil {
				loadErr.Store(err)
				return
			}
			if g.Allowed {
				atomic.AddInt32(&allowed, 1)
			} else {
				atomic.AddInt32(&denied, 1)
			}
		}()
	}
	wg.Wait()
	if e := loadErr.Load(); e != nil {
		t.Fatalf("Allow: %v", e)
	}
	if int(allowed) != capN {
		t.Fatalf("allowed=%d want %d (denied=%d)", allowed, capN, denied)
	}
	if int(denied) != total-capN {
		t.Fatalf("denied=%d want %d", denied, total-capN)
	}
}
