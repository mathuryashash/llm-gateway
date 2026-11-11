// Package metrics registers and exposes Prometheus metrics for the gateway.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// RequestsTotal counts all proxy requests by alias, provider, and HTTP status.
	RequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gateway_requests_total",
		Help: "Total proxy requests by model alias, provider, and status.",
	}, []string{"alias", "provider", "status"})

	// RequestDurationSecs tracks end-to-end gateway latency.
	RequestDurationSecs = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "gateway_request_duration_seconds",
		Help:    "End-to-end request duration in seconds.",
		Buckets: prometheus.DefBuckets,
	}, []string{"alias", "provider"})

	// UpstreamDurationSecs tracks upstream-only latency per route key.
	UpstreamDurationSecs = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "gateway_upstream_duration_seconds",
		Help:    "Upstream call latency in seconds.",
		Buckets: prometheus.DefBuckets,
	}, []string{"route_key", "status"})

	// TokensTotal counts prompt+completion tokens per provider.
	TokensTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gateway_tokens_total",
		Help: "Total tokens consumed by provider and type (prompt/completion).",
	}, []string{"provider", "token_type"})

	// CacheHitsTotal counts cache HITs.
	CacheHitsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "gateway_cache_hits_total",
		Help: "Total cache hits.",
	})

	// CacheMissesTotal counts cache MISSes.
	CacheMissesTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "gateway_cache_misses_total",
		Help: "Total cache misses.",
	})

	// FallbacksTotal counts how often the gateway fell back to a secondary provider.
	FallbacksTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gateway_fallbacks_total",
		Help: "Fallback events by route key and reason.",
	}, []string{"route_key", "reason"})

	// BreakerState tracks circuit breaker state (0=closed, 1=open, 2=half-open).
	BreakerState = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "gateway_breaker_state",
		Help: "Circuit breaker state: 0=closed 1=open 2=half_open.",
	}, []string{"route_key"})

	// RateLimitedTotal counts requests denied by the rate limiter.
	RateLimitedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gateway_rate_limited_total",
		Help: "Requests denied by rate limiter by reason.",
	}, []string{"reason"})
)
