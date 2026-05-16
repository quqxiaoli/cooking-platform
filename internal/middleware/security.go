package middleware

import "github.com/gin-gonic/gin"

// Security adds defensive HTTP response headers recommended by OWASP.
// These headers are safe for both dev and prod; no config switch needed.
func Security() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("X-Frame-Options", "DENY")
		c.Header("X-XSS-Protection", "1; mode=block")
		c.Header("Referrer-Policy", "strict-origin-when-cross-origin")
		c.Next()
	}
}
