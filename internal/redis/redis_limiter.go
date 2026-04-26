package redis

import (
	"context"
	"fmt"
	"strings"
	"time"

	redisv9 "github.com/redis/go-redis/v9"

	"github.com/shubhamjain2908/distributed-rate-limiter-v2/internal/limiter"
)

// RedisLimiter implements limiter.Limiter with a single Lua token-bucket, executed via EVALSHA (not EVAL).
type RedisLimiter struct {
	client     *redisv9.Client
	scriptSHA  string
	prefix     string
	cap        int
	perSec     float64
	now        func() time.Time
	keyName    func(string) string
	scriptText string
}

// NewRedisLimiter pre-loads the Lua with SCRIPT LOAD and records the returned SHA1 for EVALSHA.
func NewRedisLimiter(ctx context.Context, client *redisv9.Client, keyPrefix string, cap int, perSec float64) (*RedisLimiter, error) {
	if cap < 1 {
		cap = 1
	}
	if perSec <= 0 {
		perSec = 1
	}
	if keyPrefix == "" {
		keyPrefix = "rl"
	}
	sha, err := client.ScriptLoad(ctx, TokenBucketScript).Result()
	if err != nil {
		return nil, fmt.Errorf("redis limiter: script load: %w", err)
	}
	rl := &RedisLimiter{
		client:     client,
		scriptSHA:  sha,
		prefix:     keyPrefix,
		cap:        cap,
		perSec:     perSec,
		now:        time.Now,
		scriptText: TokenBucketScript,
	}
	rl.keyName = defaultKeyName(rl.prefix)
	return rl, nil
}

func defaultKeyName(prefix string) func(string) string {
	return func(clientID string) string {
		if clientID == "" {
			clientID = "anonymous"
		}
		return fmt.Sprintf("{%s}:token:%s", prefix, clientID)
	}
}

// WithKeyName sets the mapping from clientID to Redis key.
func (r *RedisLimiter) WithKeyName(fn func(string) string) *RedisLimiter {
	if fn != nil {
		r.keyName = fn
	}
	return r
}

// Allow runs the token-bucket script via EVALSHA.
func (r *RedisLimiter) Allow(ctx context.Context, clientID string, cost int) (limiter.LimitResult, error) {
	now := float64(r.now().UnixMicro()) / 1e6
	if cost < 0 {
		cost = 0
	}
	key := r.keyName(clientID)
	v, err := r.evalShaWithReload(ctx, key, r.cap, r.perSec, now, cost)
	if err != nil {
		return limiter.LimitResult{}, err
	}
	return parseRedisLimitResult(v, limiter.AlgorithmRedis)
}

func (r *RedisLimiter) evalShaWithReload(ctx context.Context, key string, args ...any) (interface{}, error) {
	v, err := r.client.EvalSha(ctx, r.scriptSHA, []string{key}, args...).Result()
	if err == nil {
		return v, nil
	}
	if isNoScriptErr(err) {
		sha, loadErr := r.client.ScriptLoad(ctx, r.scriptText).Result()
		if loadErr != nil {
			return nil, fmt.Errorf("redis limiter: reload after NOSCRIPT: %w", loadErr)
		}
		r.scriptSHA = sha
		return r.client.EvalSha(ctx, r.scriptSHA, []string{key}, args...).Result()
	}
	return nil, err
}

func isNoScriptErr(err error) bool {
	if err == nil {
		return false
	}
	// e.g. "NOSCRIPT No matching script. Please use EVAL."
	return strings.HasPrefix(err.Error(), "NOSCRIPT")
}

func parseRedisLimitResult(v interface{}, algo string) (limiter.LimitResult, error) {
	m, err := toInt64Triplet(v)
	if err != nil {
		return limiter.LimitResult{}, err
	}
	ok, rem, retryMs := m[0] == 1, int(m[1]), m[2]
	return limiter.LimitResult{
		Allowed:      ok,
		Remaining:    rem,
		RetryAfterMs: retryMs,
		Algorithm:    algo,
	}, nil
}

func toInt64Triplet(v interface{}) ([3]int64, error) {
	var out [3]int64
	switch t := v.(type) {
	case nil:
		return out, fmt.Errorf("redis limiter: empty script result")
	case []interface{}:
		if len(t) < 3 {
			return out, fmt.Errorf("redis limiter: want 3 array elements, got %d", len(t))
		}
		for i := 0; i < 3; i++ {
			out[i] = toInt64(t[i])
		}
		return out, nil
	case []int64:
		if len(t) < 3 {
			return out, fmt.Errorf("redis limiter: want 3 int64s, got %d", len(t))
		}
		copy(out[:], t)
		return out, nil
	default:
		return out, fmt.Errorf("redis limiter: unexpected result type %T", v)
	}
}

func toInt64(v interface{}) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case float64:
		return int64(n)
	case uint64:
		return int64(n)
	default:
		return 0
	}
}
