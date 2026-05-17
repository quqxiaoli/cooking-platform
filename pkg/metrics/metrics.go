// Package metrics centralises all Prometheus metric definitions for the
// cooking-platform service.
//
// Usage:
//
//	// In main(), after loading config and before starting anything else:
//	metrics.Init(cfg.Metrics.Namespace)
//
// All exported vars are nil until Init is called; callers that might run
// before Init (e.g. integration tests) should either call Init first or
// guard with a nil check — but in normal server operation Init is always
// the first call.
package metrics

import "github.com/prometheus/client_golang/prometheus"

// ── P0: HTTP ─────────────────────────────────────────────────────────────────

// HTTPRequestsTotal counts completed HTTP requests.
// Labels: handler=<gin FullPath>, method=<GET|POST|…>, status=<2xx|4xx|5xx>
var HTTPRequestsTotal *prometheus.CounterVec

// HTTPRequestDuration records end-to-end HTTP request latency.
// Buckets cover 5 ms – 5 s; P50/P95/P99 are the primary SLO metrics.
var HTTPRequestDuration *prometheus.HistogramVec

// ── P0: Consumer ─────────────────────────────────────────────────────────────

// ConsumerProcessedTotal counts events dispatched into each consumer's batch queue.
// Labels: consumer=<name>, topic=<event.TopicXxx>
var ConsumerProcessedTotal *prometheus.CounterVec

// ConsumerQueueDepth tracks the current length of each consumer's in-memory
// channel (sampled on every flush ticker tick).
// Labels: consumer=<name>
var ConsumerQueueDepth *prometheus.GaugeVec

// ── P1: Redis ─────────────────────────────────────────────────────────────────

// RedisCommandDuration records Redis round-trip latency per command.
// Labels: command=<get|set|zadd|…>, status=<ok|error>
// Populated by RedisHook registered on the go-redis client in main().
var RedisCommandDuration *prometheus.HistogramVec

// Init creates and registers all application metrics with the default
// Prometheus registry. It must be called exactly once from main() before
// any metric observation occurs.
//
// The namespace parameter is prepended to every metric name, e.g. namespace
// "cooking" produces "cooking_http_requests_total".
//
// Go runtime and process metrics (goroutine count, GC pause, CPU time, etc.)
// are provided automatically by the prometheus default registry — no explicit
// registration required here.
func Init(namespace string) {
	HTTPRequestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "http_requests_total",
			Help:      "Total HTTP requests partitioned by handler, method and status class.",
		},
		[]string{"handler", "method", "status"},
	)
	HTTPRequestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "http_request_duration_seconds",
			Help:      "HTTP request latency in seconds, partitioned by handler, method and status class.",
			Buckets:   []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5},
		},
		[]string{"handler", "method", "status"},
	)
	ConsumerProcessedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "consumer_processed_total",
			Help:      "Total events dispatched into each consumer's batch queue, by consumer name and topic.",
		},
		[]string{"consumer", "topic"},
	)
	ConsumerQueueDepth = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "consumer_queue_depth",
			Help:      "Current in-memory channel depth for each consumer (sampled on flush ticker).",
		},
		[]string{"consumer"},
	)
	RedisCommandDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "redis_command_duration_seconds",
			Help:      "Redis command round-trip latency in seconds, partitioned by command name and status.",
			Buckets:   []float64{.0001, .0005, .001, .0025, .005, .01, .025, .05, .1},
		},
		[]string{"command", "status"},
	)

	prometheus.MustRegister(
		HTTPRequestsTotal,
		HTTPRequestDuration,
		ConsumerProcessedTotal,
		ConsumerQueueDepth,
		RedisCommandDuration,
	)
}
