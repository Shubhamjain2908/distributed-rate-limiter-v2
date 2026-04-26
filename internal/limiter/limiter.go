package limiter

import "context"

// Known Algorithm values for LimitResult.Algorithm (for metrics, logs, and tests).
const (
	AlgorithmTokenBucket   = "token_bucket"
	AlgorithmSlidingWindow = "sliding_window"
	AlgorithmRedis         = "redis"
	AlgorithmInMemory      = "in_memory"
)

// LimitResult is the port-level outcome of Allow (hexagonal: adapters return this, apps interpret it).
type LimitResult struct {
	Allowed      bool
	Remaining    int
	RetryAfterMs int64
	Algorithm    string // e.g. AlgorithmTokenBucket, AlgorithmSlidingWindow, AlgorithmRedis, AlgorithmInMemory
	// ViaFallback is set when the decision is produced by the local/backup path (circuit open, or Redis error after composite tries primary).
	ViaFallback bool
}

// Limiter is the testable port: all adapters (Redis, in-memory, composite) implement this.
type Limiter interface {
	Allow(ctx context.Context, clientID string, cost int) (LimitResult, error)
}
