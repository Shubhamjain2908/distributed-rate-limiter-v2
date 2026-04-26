package redis

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	redisv9 "github.com/redis/go-redis/v9"
)

// Redis path benchmarks:
//
// - Default (miniredis in-process) is a correctness/regression stand-in; Lua in gopher-lua is
//   much slower than redis-server, so RPS is not comparable to production.
// - For wire-Redis numbers (nearer multi‑100k+ RPS on a fast path), set REDIS_BENCH_ADDR
//   (e.g. localhost:6379 with redis-server) and use -bench=BenchmarkRedisLimiter_Wire. Flushes
//   DB 15 before/after; use a dev instance only.
//
//	go test -bench=. -benchmem ./internal/redis/...
//	REDIS_BENCH_ADDR=127.0.0.1:6379 go test -bench=Wire -benchmem -count=3 ./internal/redis/...
func BenchmarkRedisLimiter_Allow_Cost1(b *testing.B) {
	ctx := context.Background()
	rl, cleanup := newMiniredisLimiterBench(b, 1<<30, 1_000_000.0)
	defer cleanup()
	benchRedisAllow(b, ctx, rl)
}

// BenchmarkRedisLimiter_Wire dials os.Getenv("REDIS_BENCH_ADDR") (e.g. 127.0.0.1:6379).
// Skips if unset. Optional DB/ FLUSHDB for isolation.
func BenchmarkRedisLimiter_Wire_Cost1(b *testing.B) {
	addr := os.Getenv("REDIS_BENCH_ADDR")
	if addr == "" {
		b.Skip("set REDIS_BENCH_ADDR (e.g. 127.0.0.1:6379) to benchmark real Redis")
	}
	ping := context.Background()
	c := redisv9.NewClient(&redisv9.Options{Addr: addr, DB: 15})
	if err := c.Ping(ping).Err(); err != nil {
		_ = c.Close()
		b.Fatalf("ping: %v", err)
	}
	_ = c.FlushDB(ping).Err()
	b.Cleanup(func() {
		_ = c.FlushDB(context.Background()).Err()
		_ = c.Close()
	})

	load, cancel := context.WithTimeout(ping, 30*time.Second)
	rl, err := NewRedisLimiter(load, c, "bwire", 1<<30, 1_000_000.0)
	cancel()
	if err != nil {
		b.Fatalf("NewRedisLimiter: %v", err)
	}
	// Monotonic "now" so the Lua state cannot stall on identical wall times; no meaningful refill
	// within the bench window.
	base := time.Unix(1_800_000_000, 0)
	var m int64
	rl.now = func() time.Time {
		m++
		return base.Add(time.Microsecond * time.Duration(m))
	}
	rl = rl.WithKeyName(func(id string) string { return "wire:" + id })
	// Limiter was constructed with a cancelled timeout ctx; the hot path uses a long-lived ctx.
	benchRedisAllow(b, ping, rl)
}

func benchRedisAllow(b *testing.B, ctx context.Context, rl *RedisLimiter) {
	const clientID = "bench"
	const cost = 1

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_, _ = rl.Allow(ctx, clientID, cost)
	}
}

func BenchmarkRedisLimiter_Allow_Cost1_Parallel(b *testing.B) {
	ctx := context.Background()
	rl, cleanup := newMiniredisLimiterBench(b, 1<<30, 1_000_000.0)
	defer cleanup()
	const clientID = "bench"
	const cost = 1

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_, _ = rl.Allow(ctx, clientID, cost)
		}
	})
}

// newMiniredisLimiterBench returns a limiter backed by miniredis and a fixed clock
// (no time-based refill in Lua).
func newMiniredisLimiterBench(b *testing.B, cap int, perSec float64) (rl *RedisLimiter, done func()) {
	b.Helper()
	m, err := miniredis.Run()
	if err != nil {
		b.Fatalf("miniredis: %v", err)
	}
	c := redisv9.NewClient(&redisv9.Options{Addr: m.Addr()})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	lim, err := NewRedisLimiter(ctx, c, "bench", cap, perSec)
	cancel()
	if err != nil {
		_ = c.Close()
		m.Close()
		b.Fatalf("NewRedisLimiter: %v", err)
	}
	sec := int64(1_000_000_000)
	lim.now = func() time.Time { return time.Unix(sec, 0) }
	lim = lim.WithKeyName(func(id string) string { return fmt.Sprintf("b:%s", id) })
	return lim, func() {
		_ = c.Close()
		m.Close()
	}
}
