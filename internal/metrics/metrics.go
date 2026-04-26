// Package metrics registers Prometheus collectors used by the HTTP layer and (optionally) adapters.
package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Registry holds counters and a dedicated Prometheus registerer.
type Registry struct {
	inner         *prometheus.Registry
	AllowedTotal  prometheus.Counter
	RejectedTotal prometheus.Counter
	ErrorsTotal   prometheus.Counter
}

// New builds a new isolated registry and metrics.
func New() *Registry {
	reg := prometheus.NewRegistry()
	allowed := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "rate_limiter_http_allowed_total",
		Help: "HTTP requests allowed after rate limit check",
	})
	rejected := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "rate_limiter_http_rejected_total",
		Help: "HTTP requests rejected (429) by rate limiter",
	})
	errs := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "rate_limiter_http_errors_total",
		Help: "HTTP requests that errored in the limiter adapter",
	})
	reg.MustRegister(allowed, rejected, errs)
	return &Registry{
		inner:         reg,
		AllowedTotal:  allowed,
		RejectedTotal: rejected,
		ErrorsTotal:   errs,
	}
}

// Handler exposes /metrics for Prometheus.
func (r *Registry) Handler() http.Handler {
	return promhttp.HandlerFor(r.inner, promhttp.HandlerOpts{Registry: r.inner})
}
