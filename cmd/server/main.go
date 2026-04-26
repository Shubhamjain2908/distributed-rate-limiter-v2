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
	m       *metrics.Registry
}

func (p *primaryThenLocal) Allow(ctx context.Context, clientID string, cost int) (limiter.LimitResult, error) {
	r := p.b.Route()
	if !r.ToPrimary {
		// Open: only local, zero Redis. Half-Open: non-probe goroutines also use local.
		res, err := p.local.Allow(ctx, clientID, cost)
		if err != nil {
			return res, err
		}
		res.ViaFallback = true
		return res, nil
	}
	start := time.Now()
	res, err := p.primary.Allow(ctx, clientID, cost)
	elapsed := time.Since(start)
	if p.m != nil {
		p.m.ObserveRedis(elapsed.Seconds())
	}
	p.b.FinishAfterPrimary(r.IsProbe, err == nil, elapsed)
	if err != nil {
		lr, lerr := p.local.Allow(ctx, clientID, cost)
		if lerr != nil {
			return lr, lerr
		}
		lr.ViaFallback = true
		return lr, nil
	}
	return res, nil
}

func main() {
	addr := getenv("SERVER_ADDR", ":8080")
	redisAddr := getenv("REDIS_ADDR", "")

	// Tuning defaults: burst 30, 10 tokens/sec. Override via env in a later step if you want.
	const burst = 30
	const perSec = 10.0

	prom := metrics.New()
	prom.SetCircuitGauges("closed") // default until a composite loop updates (all-closed = healthy)

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
				pl := &primaryThenLocal{
					b:       circuitbreaker.NewWithDefaults(),
					primary: rl,
					local:   loc,
					m:       prom,
				}
				lim = pl
				go runCircuitGauges(prom, pl)
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

	var routeCost *middleware.RouteCost
	rc, err := middleware.LoadRouteCost(getenv("COST_CONFIG_PATH", "config/cost_config.yaml"))
	if err != nil {
		log.Fatalf("route cost config: %v", err)
	}
	if rc == nil {
		log.Println("no cost_config file (COST_CONFIG_PATH), using headers + default cost only")
	} else {
		routeCost = rc
		log.Println("loaded COST_CONFIG (route > X-Request-Cost > legacy cost header > default)")
	}

	chain := prom.WrapWithPromHTTP(middleware.RateLimit(lim, prom, func(o *middleware.Options) {
		o.RouteCost = routeCost
	})(hell))
	// Register patterns so URL.Path in middleware matches config/cost_config.yaml keys.
	for _, pat := range []string{
		"GET /", "GET /search/map", "GET /search/basic", "POST /booking",
	} {
		mux.Handle(pat, chain)
	}

	log.Printf("listening on %s (set X-Client-ID for a stable per-client key; see config/cost_config.yaml)", addr)
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

func runCircuitGauges(m *metrics.Registry, p *primaryThenLocal) {
	if m == nil {
		return
	}
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for range t.C {
		m.SetCircuitGauges(circuitStateName(p.b.Snapshot()))
	}
}

func circuitStateName(s circuitbreaker.State) string {
	switch s {
	case circuitbreaker.StateOpen:
		return "open"
	case circuitbreaker.StateHalfOpen:
		return "half_open"
	default:
		return "closed"
	}
}
