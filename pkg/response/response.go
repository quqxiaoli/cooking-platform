// Package response provides a unified JSON response format for all API endpoints.
// Every response follows: {"code":0,"msg":"ok","data":{...},"request_id":"..."}
package response

import (
	"net/http"

	"cooking-platform/pkg/errcode"

	"github.com/gin-gonic/gin"
)

const requestIDKey = "X-Request-ID"

// Response is the canonical API response envelope.
type Response struct {
	Code      int         `json:"code"`
	Msg       string      `json:"msg"`
	Data      interface{} `json:"data,omitempty"`
	RequestID string      `json:"request_id"`
}

// Success writes HTTP 200 with code=0 and the provided data payload.
func Success(c *gin.Context, data interface{}) {
	c.JSON(http.StatusOK, Response{
		Code:      errcode.Success,
		Msg:       "ok",
		Data:      data,
		RequestID: getRequestID(c),
	})
}

// BadRequest writes HTTP 400 with the given AppError.
func BadRequest(c *gin.Context, err *errcode.AppError) {
	c.JSON(http.StatusBadRequest, Response{
		Code:      err.Code,
		Msg:       err.Msg,
		RequestID: getRequestID(c),
	})
}

// Unauthorized writes HTTP 401.
func Unauthorized(c *gin.Context, err *errcode.AppError) {
	c.JSON(http.StatusUnauthorized, Response{
		Code:      err.Code,
		Msg:       err.Msg,
		RequestID: getRequestID(c),
	})
}

// Forbidden writes HTTP 403.
func Forbidden(c *gin.Context, err *errcode.AppError) {
	c.JSON(http.StatusForbidden, Response{
		Code:      err.Code,
		Msg:       err.Msg,
		RequestID: getRequestID(c),
	})
}

// NotFound writes HTTP 404.
func NotFound(c *gin.Context, err *errcode.AppError) {
	c.JSON(http.StatusNotFound, Response{
		Code:      err.Code,
		Msg:       err.Msg,
		RequestID: getRequestID(c),
	})
}

// TooManyRequests writes HTTP 429.
func TooManyRequests(c *gin.Context) {
	c.JSON(http.StatusTooManyRequests, Response{
		Code:      errcode.ErrTooManyReq.Code,
		Msg:       errcode.ErrTooManyReq.Msg,
		RequestID: getRequestID(c),
	})
}

// ServerError writes HTTP 500.
func ServerError(c *gin.Context) {
	c.JSON(http.StatusInternalServerError, Response{
		Code:      errcode.ErrServer.Code,
		Msg:       errcode.ErrServer.Msg,
		RequestID: getRequestID(c),
	})
}

// Unavailable writes HTTP 503.
func Unavailable(c *gin.Context, data interface{}) {
	c.JSON(http.StatusServiceUnavailable, Response{
		Code:      errcode.ErrServiceUnavail.Code,
		Msg:       errcode.ErrServiceUnavail.Msg,
		Data:      data,
		RequestID: getRequestID(c),
	})
}

// FromError inspects the error type and writes the appropriate HTTP response.
// Handlers should use this as the single error-dispatch call.
func FromError(c *gin.Context, err error) {
	if err == nil {
		Success(c, nil)
		return
	}
	if appErr, ok := err.(*errcode.AppError); ok {
		c.JSON(appErr.HTTPStatus, Response{
			Code:      appErr.Code,
			Msg:       appErr.Msg,
			RequestID: getRequestID(c),
		})
		return
	}
	ServerError(c)
}

func getRequestID(c *gin.Context) string {
	return c.GetString(requestIDKey)
}
