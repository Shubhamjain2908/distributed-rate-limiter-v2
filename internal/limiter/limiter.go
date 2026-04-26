package limiter

import "context"

// LimitResult holds the outcome of a rate limit check.
type LimitResult struct {
	Allowed      bool
	Remaining    int
	RetryAfterMs int64
	Algorithm    string // "token_bucket" | "sliding_window"
}

// Limiter is the interface that makes everything testable.
// Both the Redis adapter and the local fallback will implement this.
type Limiter interface {
	Allow(ctx context.Context, clientID string, cost int) (LimitResult, error)
}