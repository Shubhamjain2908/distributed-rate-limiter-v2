// Package fallback is the in-memory adapter: per-client state in a sync.Map (common when the circuit is open or Redis is skipped).
package fallback

import (
	"context"
	"math"
	"sync"
	"time"

	"github.com/shubhamjain2908/distributed-rate-limiter-v2/internal/limiter"
)

// LocalLimiter implements limiter.Limiter with one token-bucket per client, sharded in sync.Map.
type LocalLimiter struct {
	m            sync.Map // string (client key) -> *clientEntry
	capacity     float64
	tokensPerSec float64
	clock        func() time.Time
	sanitize     func(string) string
}

type clientEntry struct {
	mu     sync.Mutex
	tokens float64
	last   time.Time
}

// NewLocalLimiter returns a sharded in-memory limiter. tokensPerSec and capacity match the core token-bucket semantics.
func NewLocalLimiter(capacity int, tokensPerSec float64) *LocalLimiter {
	if capacity < 1 {
		capacity = 1
	}
	if tokensPerSec <= 0 {
		tokensPerSec = 1
	}
	return &LocalLimiter{
		capacity:     float64(capacity),
		tokensPerSec: tokensPerSec,
		clock:        time.Now,
		sanitize:     defaultSanitize,
	}
}

func defaultSanitize(s string) string {
	if s == "" {
		return "anonymous"
	}
	return s
}

// WithSanitizeKey sets the clientID → key function (optional).
func (l *LocalLimiter) WithSanitizeKey(fn func(string) string) *LocalLimiter {
	if fn != nil {
		l.sanitize = fn
	}
	return l
}

// Allow runs a per-client token bucket (same math as limiter.TokenBucket, storage via sync.Map).
func (l *LocalLimiter) Allow(ctx context.Context, clientID string, cost int) (limiter.LimitResult, error) {
	if err := ctx.Err(); err != nil {
		return limiter.LimitResult{}, err
	}
	if cost < 0 {
		cost = 0
	}
	if float64(cost) > l.capacity+1e-9 {
		return limiter.LimitResult{
			Allowed:      false,
			Remaining:    0,
			RetryAfterMs: 60_000,
			Algorithm:    limiter.AlgorithmInMemory,
		}, nil
	}
	key := l.sanitize(clientID)
	now := l.clock()
	v, _ := l.m.LoadOrStore(key, &clientEntry{tokens: l.capacity, last: now})
	e := v.(*clientEntry)
	e.mu.Lock()
	defer e.mu.Unlock()
	elapsed := now.Sub(e.last)
	if elapsed > 0 {
		add := elapsed.Seconds() * l.tokensPerSec
		e.tokens = minFloat64(l.capacity, e.tokens+add)
	}
	e.last = now
	remaining := int(math.Floor(e.tokens))
	if e.tokens+1e-9 < float64(cost) {
		need := float64(cost) - e.tokens
		wait := 0.0
		if l.tokensPerSec > 0 {
			wait = need / l.tokensPerSec
		}
		retryMs := int64(math.Ceil(wait * 1000))
		if retryMs < 0 {
			retryMs = 0
		}
		return limiter.LimitResult{
			Allowed:      false,
			Remaining:    remaining,
			RetryAfterMs: retryMs,
			Algorithm:    limiter.AlgorithmInMemory,
		}, nil
	}
	e.tokens -= float64(cost)
	remaining = int(math.Floor(e.tokens))
	if remaining < 0 {
		remaining = 0
	}
	return limiter.LimitResult{
		Allowed:      true,
		Remaining:    remaining,
		RetryAfterMs: 0,
		Algorithm:    limiter.AlgorithmInMemory,
	}, nil
}

func minFloat64(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
