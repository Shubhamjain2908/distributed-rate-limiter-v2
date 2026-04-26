package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"time"

	redisv9 "github.com/redis/go-redis/v9"

	"github.com/shubhamjain2908/distributed-rate-limiter-v2/internal/circuitbreaker"
	"github.com/shubhamjain2908/distributed-rate-limiter-v2/internal/fallback"
	"github.com/shubhamjain2908/distributed-rate-limiter-v2/internal/limiter"
	"github.com/shubhamjain2908/distributed-rate-limiter-v2/internal/metrics"
	"github.com/shubhamjain2908/distributed-rate-limiter-v2/internal/middleware"
	redislim "github.com/shubhamjain2908/distributed-rate-limiter-v2/internal/redis"
)

// primaryThenLocal tries Redis (adapter) and falls back to the in-memory limiter when the breaker is open or Redis errors.
type primaryThenLocal struct {
	b       *circuitbreaker.Breaker
	primary *redislim.RedisLimiter
	local   *fallback.LocalLimiter
}

func (p *primaryThenLocal) Allow(ctx context.Context, clientID string, cost int) (limiter.LimitResult, error) {
	if !p.b.AllowRequest() {
		return p.local.Allow(ctx, clientID, cost)
	}
	res, err := p.primary.Allow(ctx, clientID, cost)
	if err != nil {
		p.b.OnFailure()
		return p.local.Allow(ctx, clientID, cost)
	}
	p.b.OnSuccess()
	return res, nil
}

func main() {
	addr := getenv("SERVER_ADDR", ":8080")
	redisAddr := getenv("REDIS_ADDR", "")

	// Tuning defaults: burst 30, 10 tokens/sec. Override via env in a later step if you want.
	const burst = 30
	const perSec = 10.0

	prom := metrics.New()

	var lim limiter.Limiter
	if redisAddr != "" {
		rc := redisv9.NewClient(&redisv9.Options{Addr: redisAddr})
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		err := rc.Ping(ctx).Err()
		cancel()
		if err != nil {
			log.Printf("REDIS_ADDR set but redis ping failed, using in-process token bucket: %v", err)
			lim = limiter.NewTokenBucket(burst, perSec)
		} else {
			loadCtx, loadCancel := context.WithTimeout(context.Background(), 2*time.Second)
			rl, serr := redislim.NewRedisLimiter(loadCtx, rc, getenv("REDIS_KEY_PREFIX", "rl"), burst, perSec)
			loadCancel()
			if serr != nil {
				log.Printf("failed to load Lua (SCRIPT load / SHA) into Redis, using in-process limiter: %v", serr)
				lim = limiter.NewTokenBucket(burst, perSec)
			} else {
				log.Println("using Redis (EVALSHA) as primary, local in-memory as fallback, circuit breaker around Redis")
				loc := fallback.NewLocalLimiter(burst, perSec)
				lim = &primaryThenLocal{
					b:       circuitbreaker.New(3, 5*time.Second),
					primary: rl,
					local:   loc,
				}
			}
		}
	} else {
		log.Println("REDIS_ADDR not set, using in-process limiter only")
		lim = limiter.NewTokenBucket(burst, perSec)
	}

	mux := http.NewServeMux()
	mux.Handle("GET /metrics", prom.Handler())
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	hell := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.Handle("GET /", middleware.RateLimit(lim, prom)(hell))

	log.Printf("listening on %s (set X-Client-ID for a stable per-client key)", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}

func getenv(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
