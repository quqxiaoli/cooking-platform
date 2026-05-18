// Package middleware — cors.go renders the CORS handshake from cfg.CORS
// (Step 18 CORS-01, Config-First). The header values must come from
// configuration so dev can stay permissive ("*") while prod ships an
// explicit allow-list — config.Validate refuses release mode + "*" at boot.
package middleware

import (
	"net/http"
	"strings"

	"cooking-platform/pkg/config"

	"github.com/gin-gonic/gin"
)

// CORS returns a Gin middleware that applies the configured CORS policy.
//
// Behaviour:
//   - cfg.AllowedOrigins == ["*"]: emit `Access-Control-Allow-Origin: *`
//     unconditionally. Dev / Docker compose use this for tooling convenience.
//   - cfg.AllowedOrigins is an explicit list: look up the request's Origin
//     header; if it matches an entry, echo it back verbatim and add
//     `Vary: Origin`. If it does not match, no Allow-Origin header is
//     written and the browser blocks the response. This is the standard
//     pattern — `Access-Control-Allow-Origin` accepts one value or `*`,
//     never a comma-separated list.
//
// Methods / headers / exposed-headers are joined with ", " per RFC 7230.
// OPTIONS preflights short-circuit with 204 so they never hit downstream
// handlers (Auth, routing). Same termination behaviour as the previous
// hard-coded implementation, just driven by config now.
func CORS(cfg config.CORSConfig) gin.HandlerFunc {
	methods := strings.Join(cfg.AllowedMethods, ", ")
	headers := strings.Join(cfg.AllowedHeaders, ", ")
	expose := strings.Join(cfg.ExposeHeaders, ", ")

	wildcard := len(cfg.AllowedOrigins) == 1 && cfg.AllowedOrigins[0] == "*"

	// Pre-build a lookup set for the explicit-list path so the per-request
	// check is O(1) instead of O(n) over a slice on every request.
	originSet := make(map[string]struct{}, len(cfg.AllowedOrigins))
	for _, o := range cfg.AllowedOrigins {
		originSet[o] = struct{}{}
	}

	return func(c *gin.Context) {
		origin := c.GetHeader("Origin")

		switch {
		case wildcard:
			c.Header("Access-Control-Allow-Origin", "*")
		case origin != "":
			if _, ok := originSet[origin]; ok {
				c.Header("Access-Control-Allow-Origin", origin)
				// Vary so caches / CDNs key responses by Origin and don't
				// serve one tenant's CORS headers to another.
				c.Header("Vary", "Origin")
			}
		}

		if methods != "" {
			c.Header("Access-Control-Allow-Methods", methods)
		}
		if headers != "" {
			c.Header("Access-Control-Allow-Headers", headers)
		}
		if expose != "" {
			c.Header("Access-Control-Expose-Headers", expose)
		}

		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}
