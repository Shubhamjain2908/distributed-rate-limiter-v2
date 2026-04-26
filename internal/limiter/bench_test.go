package limiter

import (
	"context"
	"testing"
	"time"
)

// In-process token bucket. Typical expectations on a dev laptop: multi‑M ops/sec
// (single goroutine, cost=1, fixed clock, no syscalls in the time path after WithClock).
//
//	go test -bench=. -benchmem ./internal/limiter/...
func BenchmarkTokenBucket_Allow_Cost1(b *testing.B) {
	ctx := context.Background()
	now := time.Unix(1_700_000_000, 0)
	tb := NewTokenBucket(1<<30, 1_000_000.0).WithClock(func() time.Time { return now })
	clientID := "bench"
	cost := 1

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_, _ = tb.Allow(ctx, clientID, cost)
	}
}

func BenchmarkTokenBucket_Allow_Cost1_Parallel(b *testing.B) {
	ctx := context.Background()
	now := time.Unix(1_700_000_000, 0)
	tb := NewTokenBucket(1<<30, 1_000_000.0).WithClock(func() time.Time { return now })
	clientID := "bench"
	cost := 1

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_, _ = tb.Allow(ctx, clientID, cost)
		}
	})
}
