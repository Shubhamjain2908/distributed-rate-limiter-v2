// Package middleware provides net/http middleware that calls limiter.Limiter (hexagonal: HTTP on the outside, port on the inside).
package middleware

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/shubhamjain2908/distributed-rate-limiter-v2/internal/limiter"
	"github.com/shubhamjain2908/distributed-rate-limiter-v2/internal/metrics"
)

// HeaderXRequestCost is the caller-provided per-request cost when no server route config matches.
const HeaderXRequestCost = "X-Request-Cost"

// Options configure how a client and cost are derived from a request.
type Options struct {
	// ClientIDHeader is checked first. Empty falls back to RemoteAddr (with host:port as the key).
	ClientIDHeader string
	// CostHeader optional (e.g. X-Rate-Limit-Cost) when no route and no X-Request-Cost.
	CostHeader  string
	DefaultCost int
	// RouteCost, if set, is checked first: exact "METHOD /path" from config/cost_config.yaml.
	RouteCost *RouteCost
}

// Default options: stable client id header + optional request cost.
func defaultOptions() Options {
	return Options{
		ClientIDHeader: "X-Client-ID",
		CostHeader:     "X-Rate-Limit-Cost",
		DefaultCost:    1,
	}
}

// RateLimit wraps a handler, enforcing limiter.Limiter for each request. Client ID is a quota key, not an auth check.
func RateLimit(lm limiter.Limiter, m *metrics.Registry, o ...func(*Options)) func(http.Handler) http.Handler {
	opts := defaultOptions()
	for _, f := range o {
		f(&opts)
	}
	if opts.DefaultCost < 0 {
		opts.DefaultCost = 0
	}
	if opts.ClientIDHeader == "" {
		opts.ClientIDHeader = "X-Client-ID"
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if m != nil {
				m.IncSLORequest()
			}
			clientID := clientIDFromRequest(r, opts.ClientIDHeader)
			cost := ResolveCost(r, &opts)
			res, err := lm.Allow(r.Context(), clientID, cost)
			if err != nil {
				if m != nil {
					m.IncSLOError()
				}
				http.Error(w, "rate limiter error", http.StatusInternalServerError)
				return
			}
			if m != nil {
				if res.ViaFallback {
					m.IncFallback()
				}
				m.IncDecision(res.Allowed, res.ViaFallback, res.Algorithm)
			}
			w.Header().Set("X-Rate-Limit-Remaining", strconv.Itoa(res.Remaining))
			if res.Algorithm != "" {
				w.Header().Set("X-Rate-Limit-Algorithm", res.Algorithm)
			}
			if !res.Allowed {
				if res.RetryAfterMs > 0 {
					sec := (res.RetryAfterMs + 999) / 1000
					if sec < 1 {
						sec = 1
					}
					w.Header().Set("Retry-After", strconv.FormatInt(sec, 10))
				}
				http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func clientIDFromRequest(r *http.Request, headerName string) string {
	h := r.Header.Get(headerName)
	if strings.TrimSpace(h) != "" {
		return strings.TrimSpace(h)
	}
	return r.RemoteAddr
}
