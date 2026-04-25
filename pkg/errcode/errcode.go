// Package errcode defines all application-level error codes and the AppError type.
// Convention: XYYZZZ where X=category(4=client,5=server), YY=module, ZZZ=sequence.
package errcode

import (
	"fmt"
	"net/http"
)

// AppError is a structured error carrying an HTTP status, business code, and message.
type AppError struct {
	HTTPStatus int    `json:"-"`
	Code       int    `json:"code"`
	Msg        string `json:"msg"`
}

func (e *AppError) Error() string {
	return fmt.Sprintf("code=%d msg=%s", e.Code, e.Msg)
}

// New creates an AppError. Use predefined constants below rather than calling this directly.
func New(httpStatus, code int, msg string) *AppError {
	return &AppError{HTTPStatus: httpStatus, Code: code, Msg: msg}
}

// ── Success ─────────────────────────────────────────────────────────────────
const Success = 0

// ── 4xx Client Errors ───────────────────────────────────────────────────────
var (
	ErrInvalidParams  = New(http.StatusBadRequest, 400001, "invalid request parameters")
	ErrUnauthorized   = New(http.StatusUnauthorized, 401001, "unauthorized")
	ErrTokenExpired   = New(http.StatusUnauthorized, 401002, "token expired")
	ErrTokenInvalid   = New(http.StatusUnauthorized, 401003, "token invalid")
	ErrForbidden      = New(http.StatusForbidden, 403001, "forbidden")
	ErrNotFound       = New(http.StatusNotFound, 404001, "resource not found")
	ErrTooManyReq     = New(http.StatusTooManyRequests, 429001, "too many requests")
)

// ── 5xx Server Errors ───────────────────────────────────────────────────────
var (
	ErrServer         = New(http.StatusInternalServerError, 500001, "internal server error")
	ErrDatabase       = New(http.StatusInternalServerError, 500002, "database error")
	ErrCacheError     = New(http.StatusInternalServerError, 500003, "cache error")
	ErrServiceUnavail = New(http.StatusServiceUnavailable, 503001, "service unavailable")
)
