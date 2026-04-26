// Package metrics registers SLO metrics (counter, histogram, gauge) and the Prometheus registry.
// Error budget: emit ratelimiter_slo_{requests,errors}_total for 28d PromQL; the gauge
// ratelimiter_error_budget_remaining is the process-lifetime ratio (1 - errors/requests) for a live readout.
package metrics

import (
	"math"
	"net/http"
	"strings"
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Registry holds the five headline SLO series plus SLO request/error counters for 28d windows, and promhttp HTTP metrics.
type Registry struct {
	inner *prometheus.Registry

	Decisions         *prometheus.CounterVec
	RedisDuration     prometheus.Histogram
	CircuitState      *prometheus.GaugeVec
	FallbackDecisions prometheus.Counter
	SloRequests       prometheus.Counter
	SloErrors         prometheus.Counter

	HTTPInFlight prometheus.Gauge
	HTTPDuration *prometheus.HistogramVec // for promhttp.InstrumentHandlerDuration; no extra labels
	sloErrAtomic atomic.Uint64
	sloReqAtomic atomic.Uint64
}

// New builds a new isolated registry, registers all SLOs and HTTP promhttp series.
func New() *Registry {
	reg := prometheus.NewRegistry()

	// 1) ratelimiter_decisions_total{result, algorithm}
	decisions := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "ratelimiter_decisions_total",
		Help: "Per-request rate limit decisions. result=fallback when local path was used (degraded) while using Redis+composite; otherwise allowed or rejected (algorithm=implementation).",
	}, []string{"result", "algorithm"})

	// 2) SLO: p99 < 2ms; alert when p99 > 10ms — histogram in seconds, tight buckets for sub-10ms
	redisH := prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "ratelimiter_redis_duration_seconds",
		Help:    "Time spent in Redis (EVALSHA) for one Allow call. SLO p99 < 0.002s. Alert: p99 > 0.01s",
		Buckets: []float64{0.0001, 0.0002, 0.0005, 0.001, 0.0015, 0.002, 0.003, 0.005, 0.01, 0.02, 0.1, 1.0},
	})

	// 3) ratelimiter_circuit_state{state=closed|open|half_open} — exactly one 1, others 0
	circ := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "ratelimiter_circuit_state",
		Help: "1 if the FSM is in that state, else 0. Alert: open for more than 60s (Grafana: min_over_time[...]).",
	}, []string{"state"})

	// 4) fallback decisions
	fallback := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "ratelimiter_fallback_decisions_total",
		Help: "Decisions served from the local in-memory limiter (circuit open, or after Redis error).",
	})

	// Counters for 28d / recording rules: 1 - increase(errors[28d]) / increase(requests[28d])
	sloReq := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "ratelimiter_slo_requests_total",
		Help: "Total Allow attempts at the limiter (denominator for error budget; use with _errors_ for 28d PromQL).",
	})
	sloErr := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "ratelimiter_slo_errors_total",
		Help: "Allow calls that failed with error (5xx from limiter path) — SLO error numerator.",
	})

	// 5) gauge: process error budget = 1 - (errors/requests) since start (interview: same ratio, rolling 28d in Grafana from counters)
	r := &Registry{
		inner:         reg,
		Decisions:     decisions,
		RedisDuration: redisH,
		CircuitState:  circ,
	}
	r.SloRequests = sloReq
	r.SloErrors = sloErr
	r.sloErrAtomic = atomic.Uint64{}
	r.sloReqAtomic = atomic.Uint64{}
	r.FallbackDecisions = fallback

	budgetG := prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{
			Name: "ratelimiter_error_budget_remaining",
			Help: "Process-lifetime: 1 - (errors/requests) while running. For 28d error budget, use: 1 - (sum(increase(ratelimiter_slo_errors_total[28d]))/max(sum(increase(ratelimiter_slo_requests_total[28d])),1)) in Grafana/Alertmanager.",
		},
		r.errorBudgetValue,
	)

	httpInflight := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "http_handler_requests_in_flight",
		Help: "In-flight requests (promhttp.InstrumentHandlerInFlight).",
	})
	httpHist := prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "http_handler_request_duration_seconds",
			Help:    "HTTP request duration in seconds (promhttp.InstrumentHandlerDuration, unlabelled).",
			Buckets: prometheus.DefBuckets,
		},
		nil,
	)
	r.HTTPInFlight = httpInflight
	r.HTTPDuration = httpHist

	reg.MustRegister(
		decisions, redisH, circ, fallback, sloReq, sloErr, budgetG,
		httpInflight, httpHist,
	)
	return r
}

func (r *Registry) errorBudgetValue() float64 {
	req := r.sloReqAtomic.Load()
	err := r.sloErrAtomic.Load()
	if req == 0 {
		return 1.0
	}
	ef := float64(err) / float64(req)
	return 1.0 - math.Min(1.0, math.Max(0, ef))
}

// IncSLORequest increments the request counter and tracking for the error budget gauge.
func (r *Registry) IncSLORequest() {
	if r == nil {
		return
	}
	r.SloRequests.Inc()
	r.sloReqAtomic.Add(1)
}

// IncSLOError records a failed Allow (5xx) for SLO burn.
func (r *Registry) IncSLOError() {
	if r == nil {
		return
	}
	r.SloErrors.Inc()
	r.sloErrAtomic.Add(1)
}

// IncDecision maps LimitResult to ratelimiter_decisions_total{result, algorithm}.
// algorithm is normalized: token_bucket|sliding_window|redis|in_memory|unknown
func (r *Registry) IncDecision(allowed, viaFallback bool, algorithm string) {
	if r == nil {
		return
	}
	algo := normalizeAlgorithm(algorithm)
	var res string
	switch {
	case viaFallback:
		res = "fallback"
	case allowed:
		res = "allowed"
	default:
		res = "rejected"
	}
	r.Decisions.WithLabelValues(res, algo).Inc()
}

// IncFallback increments the fallback path counter.
func (r *Registry) IncFallback() {
	if r == nil {
		return
	}
	r.FallbackDecisions.Inc()
}

// ObserveRedis records one Redis EVALSHA duration (seconds).
func (r *Registry) ObserveRedis(d float64) {
	if r == nil {
		return
	}
	r.RedisDuration.Observe(d)
}

// SetCircuitGauges sets 1 for the active FSM state and 0 for the others. States: "closed", "open", "half_open".
func (r *Registry) SetCircuitGauges(stateName string) {
	if r == nil {
		return
	}
	closed, open, half := 0.0, 0.0, 0.0
	switch stateName {
	case "closed":
		closed = 1
	case "open":
		open = 1
	case "half_open":
		half = 1
	}
	r.CircuitState.WithLabelValues("closed").Set(closed)
	r.CircuitState.WithLabelValues("open").Set(open)
	r.CircuitState.WithLabelValues("half_open").Set(half)
}

func normalizeAlgorithm(a string) string {
	switch a {
	case "token_bucket", "sliding_window", "redis", "in_memory":
		return a
	}
	// also accept from internal/limiter const raw strings
	s := strings.ToLower(strings.ReplaceAll(a, " ", ""))
	if s == "" {
		return "unknown"
	}
	if strings.Contains(s, "sliding") {
		return "sliding_window"
	}
	if strings.Contains(s, "token") {
		return "token_bucket"
	}
	if strings.Contains(s, "memory") {
		return "in_memory"
	}
	return "unknown"
}

// PromRegistry returns the low-level registerer.
func (r *Registry) PromRegistry() *prometheus.Registry {
	if r == nil {
		return nil
	}
	return r.inner
}

// Handler exposes /metrics.
func (r *Registry) Handler() http.Handler {
	if r == nil {
		return promhttp.Handler()
	}
	return promhttp.HandlerFor(r.inner, promhttp.HandlerOpts{Registry: r.inner})
}

// WrapWithPromHTTP wraps a handler with promhttp InstrumentHandlerInFlight and InstrumentHandlerDuration
// (standard Prometheus HTTP instrumentation on the same registry as SLOs).
func (r *Registry) WrapWithPromHTTP(h http.Handler) http.Handler {
	if r == nil {
		return h
	}
	// unpartitioned histogram: zero label names
	curried, err := r.HTTPDuration.CurryWith(prometheus.Labels{})
	if err != nil {
		return h
	}
	inner := promhttp.InstrumentHandlerInFlight(r.HTTPInFlight, h)
	return promhttp.InstrumentHandlerDuration(curried, inner)
}
