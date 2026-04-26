package limiter

import (
	"context"
	"math"
	"sync"
	"time"
)

// TokenBucket is a pure-Go, in-process token bucket: refill rate, burst capacity, per clientID.
type TokenBucket struct {
	mu sync.Mutex
	// clients maps clientID -> per-client state.
	clients       map[string]*tokenBucketState
	capacity      float64
	tokensPerSec  float64
	clock         func() time.Time
	sanitizeKeyFn func(string) string
}

type tokenBucketState struct {
	tokens     float64
	lastUpdate time.Time
}

// NewTokenBucket returns a multi-tenant token bucket. tokensPerSec is continuous refill; capacity is burst.
func NewTokenBucket(capacity int, tokensPerSec float64) *TokenBucket {
	if capacity < 1 {
		capacity = 1
	}
	if tokensPerSec <= 0 {
		tokensPerSec = 1
	}
	return &TokenBucket{
		clients:       make(map[string]*tokenBucketState),
		capacity:      float64(capacity),
		tokensPerSec:  tokensPerSec,
		clock:         time.Now,
		sanitizeKeyFn: defaultSanitize,
	}
}

// WithSanitizeKey sets a key normalizer (optional). Used to align keys with Redis key rules.
func (t *TokenBucket) WithSanitizeKey(fn func(string) string) *TokenBucket {
	t.sanitizeKeyFn = fn
	return t
}

func defaultSanitize(s string) string {
	if s == "" {
		return "anonymous"
	}
	return s
}

func (t *TokenBucket) Allow(ctx context.Context, clientID string, cost int) (LimitResult, error) {
	if err := ctx.Err(); err != nil {
		return LimitResult{}, err
	}
	if cost < 0 {
		cost = 0
	}
	key := t.sanitizeKeyFn(clientID)
	if float64(cost) > t.capacity+1e-9 {
		return LimitResult{
			Allowed:      false,
			Remaining:    0,
			RetryAfterMs: 60_000,
			Algorithm:    AlgorithmTokenBucket,
		}, nil
	}
	now := t.clock()
	t.mu.Lock()
	defer t.mu.Unlock()

	st, ok := t.clients[key]
	if !ok {
		st = &tokenBucketState{tokens: t.capacity, lastUpdate: now}
		t.clients[key] = st
	}
	// Refill: tokens += elapsed * rate
	elapsed := now.Sub(st.lastUpdate)
	if elapsed > 0 {
		add := elapsed.Seconds() * t.tokensPerSec
		st.tokens = minFloat64(t.capacity, st.tokens+add)
		st.lastUpdate = now
	}
	remaining := int(math.Floor(st.tokens))
	if st.tokens+1e-9 < float64(cost) {
		need := float64(cost) - st.tokens
		waitSec := 0.0
		if t.tokensPerSec > 0 {
			waitSec = need / t.tokensPerSec
		}
		retryMs := int64(math.Ceil(waitSec * 1000))
		if retryMs < 0 {
			retryMs = 0
		}
		return LimitResult{
			Allowed:      false,
			Remaining:    remaining,
			RetryAfterMs: retryMs,
			Algorithm:    AlgorithmTokenBucket,
		}, nil
	}
	st.tokens -= float64(cost)
	remaining = int(math.Floor(st.tokens))
	if remaining < 0 {
		remaining = 0
	}
	return LimitResult{
		Allowed:      true,
		Remaining:    remaining,
		RetryAfterMs: 0,
		Algorithm:    AlgorithmTokenBucket,
	}, nil
}

func minFloat64(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
