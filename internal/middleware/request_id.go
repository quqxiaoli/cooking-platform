// Package middleware contains gin middleware components used across all routes.
package middleware

import (
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const requestIDKey = "X-Request-ID"

// RequestID injects a unique request identifier into every incoming request.
// It first checks the X-Request-ID header (useful for tracing across services),
// falls back to generating a new UUID v4. The ID is:
//   - stored in gin.Context (c.Get("X-Request-ID"))
//   - written to the response header so clients can correlate logs
func RequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.GetHeader(requestIDKey)
		if id == "" {
			id = uuid.NewString()
		}
		c.Set(requestIDKey, id)
		c.Header(requestIDKey, id)
		c.Next()
	}
}
