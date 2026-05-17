package middleware

import (
	"time"

	"cooking-platform/pkg/metrics"

	"github.com/gin-gonic/gin"
)

// Metrics returns a Gin middleware that records per-request Prometheus metrics:
//
//   - cooking_http_requests_total{handler, method, status}
//   - cooking_http_request_duration_seconds{handler, method, status}
//
// handler is c.FullPath() (the parameterised route pattern, e.g. "/api/v1/posts/:id")
// so high-cardinality path params (:id) don't explode the label space.
// Requests that match no route get handler="unknown".
//
// status is a class string: "2xx", "4xx", "5xx" (not the raw integer).
// Grouping by class keeps cardinality manageable on dashboards.
//
// This middleware must be registered AFTER Recovery() so panic-recovered
// requests are counted with the correct status code (500) rather than 0.
func Metrics() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()

		if metrics.HTTPRequestsTotal == nil {
			return // metrics.Init() not called (test environments)
		}

		handler := c.FullPath()
		if handler == "" {
			handler = "unknown"
		}
		method := c.Request.Method
		status := httpStatusClass(c.Writer.Status())

		elapsed := time.Since(start).Seconds()
		metrics.HTTPRequestsTotal.WithLabelValues(handler, method, status).Inc()
		metrics.HTTPRequestDuration.WithLabelValues(handler, method, status).Observe(elapsed)
	}
}

// httpStatusClass converts an HTTP status code to a broad class string.
// Using a class instead of the raw integer bounds label cardinality.
func httpStatusClass(code int) string {
	switch {
	case code < 200:
		return "1xx"
	case code < 300:
		return "2xx"
	case code < 400:
		return "3xx"
	case code < 500:
		return "4xx"
	default:
		return "5xx"
	}
}
