package middleware

import (
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// Logger logs each HTTP request using the global zap logger.
// Fields logged: method, path, status, latency, client_ip, request_id.
// Errors are logged at Error level; everything else at Info level.
func Logger() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		path := c.Request.URL.Path
		query := c.Request.URL.RawQuery

		c.Next()

		latency := time.Since(start)
		status := c.Writer.Status()
		requestID := c.GetString(requestIDKey)

		fields := []zap.Field{
			zap.String("request_id", requestID),
			zap.String("method", c.Request.Method),
			zap.String("path", path),
			zap.String("query", query),
			zap.Int("status", status),
			zap.Duration("latency", latency),
			zap.String("client_ip", c.ClientIP()),
			zap.String("user_agent", c.Request.UserAgent()),
		}

		if len(c.Errors) > 0 {
			for _, e := range c.Errors {
				zap.L().Error("request error", append(fields, zap.Error(e.Err))...)
			}
			return
		}

		if status >= 500 {
			zap.L().Error("server error", fields...)
		} else if status >= 400 {
			zap.L().Warn("client error", fields...)
		} else {
			zap.L().Info("request", fields...)
		}
	}
}
