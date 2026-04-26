package redis

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	redisv9 "github.com/redis/go-redis/v9"
)

// Table-driven, miniredis, fixed wall clock. Lua uses UnixMicro/1e6; we advance per whole seconds.
func TestTokenBucket_SequentialAllows(t *testing.T) {
	t.Parallel()
	sec := int64(1_000_000_000) // ~2001, valid Unix seconds
	rl, cleanup, _ := newRedisTest(t, 10, 10.0, &sec) // same Unix second → no refill between table rows
	defer cleanup()
	ctx := context.Background()

	rows := []struct {
		cost  int
		remW  int
		allow bool
	}{
		{1, 9, true},
		{1, 8, true},
		{5, 3, true},
		{0, 3, true},
		{1, 2, true},
		{1, 1, true},
		{1, 0, true},
		{1, 0, false},
	}
	for i, r := range rows {
		g, err := rl.Allow(ctx, "client1", r.cost)
		if err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		if g.Allowed != r.allow {
			t.Fatalf("step %d: allowed=%v want %v (rem=%d)", i, g.Allowed, r.allow, g.Remaining)
		}
		if g.Allowed && g.Remaining != r.remW {
			t.Fatalf("step %d: rem=%d want %d", i, g.Remaining, r.remW)
		}
		if !g.Allowed && g.Remaining != r.remW {
			// Deny path: Lua returns rem = floor(min(cap, tokens)) before write
			t.Fatalf("step %d: on deny, rem=%d want %d", i, g.Remaining, r.remW)
		}
	}
}

// Refill: drain partially, advance 1s, allow should see full top-up to cap then deduct.
func TestTokenBucket_Refill(t *testing.T) {
	t.Parallel()
	sec := int64(1_000_000_000)
	rl, cleanup, secPtr := newRedisTest(t, 10, 10, &sec)
	defer cleanup()
	ctx := context.Background()

	g1, err := rl.Allow(ctx, "c", 5) // 5 rem
	if err != nil {
		t.Fatal(err)
	}
	if g1.Remaining != 5 {
		t.Fatalf("rem=%d want 5", g1.Remaining)
	}
	*secPtr += 1 // +1s: refill 10 tokens: min(10, 5+10) = 10, then cost 1 => 9

	g2, err := rl.Allow(ctx, "c", 1)
	if err != nil {
		t.Fatal(err)
	}
	if !g2.Allowed || g2.Remaining != 9 {
		t.Fatalf("after refill+deduct: %+v, want rem=9", g2)
	}
}

// If SCRIPT FLUSH, next EVALSHA reloads (NOSCRIPT path).
func TestEvalSha_ReloadsAfterNOScript(t *testing.T) {
	t.Parallel()
	sec := int64(1_000_000_000)
	rl, cleanup, _ := newRedisTest(t, 2, 1, &sec)
	defer cleanup()
	ctx := context.Background()

	g, err := rl.Allow(ctx, "c", 1)
	if err != nil || !g.Allowed {
		t.Fatalf("first: err=%v g=%+v", err, g)
	}
	if err := rl.client.ScriptFlush(ctx).Err(); err != nil {
		t.Fatal(err)
	}
	g2, err := rl.Allow(ctx, "c", 0)
	if err != nil {
		t.Fatalf("after flush: %v", err)
	}
	if !g2.Allowed {
		t.Fatalf("expected reload then success: %+v", g2)
	}
}

// miniredis + NewRedisLimiter, stable key, time = Unix(*sec, 0).
func newRedisTest(t *testing.T, cap int, perSec float64, sec *int64) (rl *RedisLimiter, done func(), secOut *int64) {
	t.Helper()
	m := miniredis.RunT(t)
	c := redisv9.NewClient(&redisv9.Options{Addr: m.Addr()})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	lim, err := NewRedisLimiter(ctx, c, "ut", cap, perSec)
	cancel()
	if err != nil {
		t.Fatalf("NewRedisLimiter: %v", err)
	}
	lim.now = func() time.Time { return time.Unix(*sec, 0) }
	lim = lim.WithKeyName(func(id string) string { return fmt.Sprintf("ut:%s", id) })
	return lim, func() { _ = c.Close() }, sec
}
