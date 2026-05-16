package middleware

import (
	"net/url"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// sensitiveQueryParams lists query parameter names whose values must be
// redacted before writing to logs. Names are matched case-insensitively.
var sensitiveQueryParams = map[string]struct{}{
	"phone": {}, "mobile": {}, "code": {}, "token": {},
	"access_key": {}, "accesskey": {}, "secret": {}, "password": {},
}

// sanitizeQuery replaces values of sensitive query parameters with "***".
func sanitizeQuery(raw string) string {
	if raw == "" {
		return ""
	}
	parsed, err := url.ParseQuery(raw)
	if err != nil {
		return "[unparseable]"
	}
	for key := range parsed {
		lower := key
		for i := 0; i < len(lower); i++ {
			if lower[i] >= 'A' && lower[i] <= 'Z' {
				lower = lower[:i] + string(lower[i]+'a'-'A') + lower[i+1:]
			}
		}
		if _, sensitive := sensitiveQueryParams[lower]; sensitive {
			parsed[key] = []string{"***"}
		}
	}
	return parsed.Encode()
}

// Logger logs each HTTP request using the global zap logger.
// Fields logged: method, path, status, latency, client_ip, request_id.
// Query parameters matching sensitiveQueryParams are redacted to "***".
func Logger() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		path := c.Request.URL.Path
		query := sanitizeQuery(c.Request.URL.RawQuery)

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
