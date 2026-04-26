package redis

import (
	"context"
	"os"
	"sort"
	"testing"
	"time"

	redisv9 "github.com/redis/go-redis/v9"
)

// TestWireAllow_MeasureOneClient records per-Allow latency against real Redis (same setup as
// BenchmarkRedisLimiter_Wire). Skips if REDIS_BENCH_ADDR is unset. Run locally to refresh README
// table numbers:  REDIS_BENCH_ADDR=127.0.0.1:6379 go test -v -run TestWireAllow_MeasureOneClient ./internal/redis/
func TestWireAllow_MeasureOneClient(t *testing.T) {
	addr := os.Getenv("REDIS_BENCH_ADDR")
	if addr == "" {
		t.Skip("set REDIS_BENCH_ADDR (e.g. 127.0.0.1:6379) and start redis-server to measure")
	}
	ping := context.Background()
	c := redisv9.NewClient(&redisv9.Options{Addr: addr, DB: 15})
	if err := c.Ping(ping).Err(); err != nil {
		_ = c.Close()
		t.Fatalf("ping: %v", err)
	}
	if err := c.FlushDB(ping).Err(); err != nil {
		_ = c.Close()
		t.Fatalf("flush: %v", err)
	}
	t.Cleanup(func() {
		_ = c.FlushDB(context.Background()).Err()
		_ = c.Close()
	})
	load, cancel := context.WithTimeout(ping, 30*time.Second)
	rl, err := NewRedisLimiter(load, c, "bwire", 1<<30, 1_000_000.0)
	cancel()
	if err != nil {
		t.Fatalf("NewRedisLimiter: %v", err)
	}
	base := time.Unix(1_800_000_000, 0)
	var m int64
	rl.now = func() time.Time {
		m++
		return base.Add(time.Microsecond * time.Duration(m))
	}
	rl = rl.WithKeyName(func(id string) string { return "wiresample:" + id })

	const n = 20_000
	durs := make([]time.Duration, 0, n)
	for i := 0; i < n; i++ {
		t0 := time.Now()
		_, err := rl.Allow(ping, "one", 1)
		if err != nil {
			t.Fatalf("allow %d: %v", i, err)
		}
		durs = append(durs, time.Since(t0))
	}
	nanos := make([]int64, n)
	for i, d := range durs {
		nanos[i] = d.Nanoseconds()
	}
	sort.Slice(nanos, func(i, j int) bool { return nanos[i] < nanos[j] })
	p50 := time.Duration(nanos[n*50/100])
	p99 := time.Duration(nanos[n*99/100])
	var sum int64
	for _, v := range nanos {
		sum += v
	}
	meanNs := float64(sum) / float64(n)
	meanRPS := 1e9 / meanNs

	t.Logf("n=%d single goroutine, one clientID, EVALSHA path, Redis @ %s", n, addr)
	t.Logf("p50=%s  p99=%s  mean=%s  ≈one-client RPS=%.0f (from mean RTT, not aggregate parallel)",
		p50, p99, time.Duration(int64(meanNs)), meanRPS)
}
