package middleware

import (
	"net/http"

	"cooking-platform/pkg/errcode"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// Recovery catches panics in any handler, logs the full stack trace,
// and returns a clean HTTP 500 response instead of crashing the server.
func Recovery() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if r := recover(); r != nil {
				zap.L().Error("panic recovered",
					zap.Any("error", r),
					zap.String("request_id", c.GetString(requestIDKey)),
					zap.String("method", c.Request.Method),
					zap.String("path", c.Request.URL.Path),
				)
				c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{
					"code":       errcode.ErrServer.Code,
					"msg":        errcode.ErrServer.Msg,
					"request_id": c.GetString(requestIDKey),
				})
			}
		}()
		c.Next()
	}
}
