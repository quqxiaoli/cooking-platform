// Package errcode defines all application-level error codes and the AppError type.
//
// Numbering scheme: XYYZZZ
//   - X   : code-segment prefix
//       - 4 : client-facing modules (10=user, 12=post, 40=follow, 50=search,
//             60=upload), plus the general HTTP-4XX bucket (00=general).
//       - 5 : server-internal / infra modules whose HTTP status is fixed at
//             500 or 503 regardless of which module raised them.
//             Today: 00=general (500xxx internal / 503xxx unavailable),
//             but historically 470xxx and 480xxx also live under the X=4
//             prefix and are also HTTP 500. The single source of truth for
//             the HTTP status of an error is AppError.HTTPStatus, never the
//             X digit.
//   - YY  : module
//   - ZZZ : per-module sequence
//
// Note on the X=4 vs X=5 inconsistency: audit (470xxx) and encryption (480xxx)
// were originally placed under X=4 to keep all module codes co-located, even
// though they are always HTTP 500. New 5xx-only modules SHOULD use X=5
// (e.g. ErrServiceUnavail = 503001 lives in the X=5 bucket). The lesson is
// "HTTP status is determined by AppError.HTTPStatus, not by the X digit".
//
// Audit (470xxx) and Encryption (480xxx) codes are never returned to HTTP
// callers — they exist for structured log tagging and Prometheus alerting only.
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

// ── General (code segment 400xxx / 401xxx / 403xxx / 404xxx / 429xxx / 500xxx / 503xxx) ──
var (
	ErrInvalidParams = New(http.StatusBadRequest, 400001, "invalid request parameters")
	ErrUnauthorized  = New(http.StatusUnauthorized, 401001, "unauthorized")
	ErrTokenExpired  = New(http.StatusUnauthorized, 401002, "token expired")
	ErrTokenInvalid  = New(http.StatusUnauthorized, 401003, "token invalid")
	ErrForbidden     = New(http.StatusForbidden, 403001, "forbidden")
	ErrNotFound      = New(http.StatusNotFound, 404001, "resource not found")
	ErrTooManyReq    = New(http.StatusTooManyRequests, 429001, "too many requests")
	ErrServer        = New(http.StatusInternalServerError, 500001, "internal server error")
	ErrDatabase      = New(http.StatusInternalServerError, 500002, "database error")
	ErrCacheError    = New(http.StatusInternalServerError, 500003, "cache error")
	ErrServiceUnavail = New(http.StatusServiceUnavailable, 503001, "service unavailable")
)

// ErrServiceUnavailable is an alias for ErrServiceUnavail kept for naming
// symmetry with ErrServiceUnavailable across other places in the codebase
// (handlers / docs may refer to either name). New code should pick whichever
// reads better — both point at the same 503/503001 envelope.
var ErrServiceUnavailable = ErrServiceUnavail

// ── User Module (code segment 410xxx) ───────────────────────────────────────
var (
	ErrPhoneFormat      = New(http.StatusBadRequest, 410101, "invalid phone number format")
	ErrCodeFormat       = New(http.StatusBadRequest, 410102, "invalid verification code format")
	ErrCodeNotFound     = New(http.StatusBadRequest, 410103, "verification code not found or expired")
	ErrCodeMismatch     = New(http.StatusBadRequest, 410104, "verification code does not match")
	ErrSMSWindow        = New(http.StatusTooManyRequests, 410105, "send code too frequently, please wait")
	ErrSMSDailyPhone    = New(http.StatusTooManyRequests, 410106, "daily send limit reached for this phone")
	ErrSMSDailyIP       = New(http.StatusTooManyRequests, 410107, "daily send limit reached for this IP")
	ErrUserNotFound     = New(http.StatusNotFound, 410108, "user not found")
	ErrUserBanned       = New(http.StatusForbidden, 410109, "user is banned")
	ErrNicknameInvalid  = New(http.StatusBadRequest, 410110, "invalid nickname")
	ErrBioTooLong       = New(http.StatusBadRequest, 410111, "bio is too long")
	ErrAvatarURLInvalid = New(http.StatusBadRequest, 410112, "invalid avatar URL")
)

// ── Post Module (code segment 412xxx) ───────────────────────────────────────
// 411xxx is reserved for future user-extension (account recovery, device mgmt).
var (
	ErrTitleEmpty      = New(http.StatusBadRequest, 412101, "title cannot be empty")
	ErrTitleTooLong    = New(http.StatusBadRequest, 412102, "title exceeds 100 characters")
	ErrSceneTagInvalid = New(http.StatusBadRequest, 412103, "scene_tag must be between 1 and 8")
	ErrPostNotFound    = New(http.StatusNotFound, 412104, "post not found")
	ErrPostForbidden   = New(http.StatusForbidden, 412105, "no permission for this post")
	ErrCursorInvalid   = New(http.StatusBadRequest, 412106, "invalid cursor")
	ErrPageSizeInvalid = New(http.StatusBadRequest, 412107, "page size must be between 1 and 50")
	ErrContentTooLong  = New(http.StatusBadRequest, 412108, "content exceeds 5000 characters")
)

// ── Follow Module (code segment 440xxx) ─────────────────────────────────────
// Reused codes: ErrUserNotFound (410108), ErrUnauthorized (401001).
var (
	ErrCannotFollowSelf    = New(http.StatusBadRequest, 440101, "cannot follow yourself")
	ErrFollowLimitExceeded = New(http.StatusBadRequest, 440102, "following limit reached (max 3000)")
	ErrFollowNotFound      = New(http.StatusNotFound, 440103, "follow relationship not found")
	ErrFollowCursorInvalid = New(http.StatusBadRequest, 440104, "invalid follow list cursor")
)

// ── Search Module (code segment 450xxx) ─────────────────────────────────────
// Reused codes: ErrTooManyReq (429001).
var (
	ErrSearchKeywordEmpty  = New(http.StatusBadRequest, 450101, "search keyword cannot be empty")
	ErrSearchCursorInvalid = New(http.StatusBadRequest, 450102, "invalid search cursor")
)

// ── Upload Module (code segment 460xxx) ─────────────────────────────────────
// Reused codes: ErrUnauthorized (401001), ErrTooManyReq (429001).
var (
	ErrUploadFileType        = New(http.StatusBadRequest, 460101, "unsupported file type")
	ErrUploadFileTooLarge    = New(http.StatusBadRequest, 460102, "file exceeds size limit")
	ErrUploadPresignFailed   = New(http.StatusInternalServerError, 460103, "failed to generate presigned url")
	ErrUploadCallbackInvalid = New(http.StatusBadRequest, 460104, "invalid callback nonce")
	ErrUploadURLNotAllowed   = New(http.StatusBadRequest, 460105, "url not in oss whitelist")
	ErrPostStepsInvalid      = New(http.StatusBadRequest, 460106, "post steps invalid")
)

// ── Audit Module (code segment 470xxx, HTTP 500) ─────────────────────────────
// Internal errors emitted by AuditConsumer. Never returned to HTTP callers —
// audit runs fully async. Codes exist for structured log tagging so
// Prometheus / Grafana can alert on audit pipeline failures.
var (
	ErrAuditAPIFailed   = New(http.StatusInternalServerError, 470101, "content safety API call failed")
	ErrAuditWriteFailed = New(http.StatusInternalServerError, 470102, "audit result write failed")
)

// ── Encryption Module (code segment 480xxx, HTTP 500) ────────────────────────
// Internal errors for AES-GCM phone field-level encryption. Never returned to
// HTTP callers — encryption is transparent infrastructure. Codes exist for
// structured log tagging (Prometheus alert on key misconfiguration).
var (
	ErrEncryptPhone    = New(http.StatusInternalServerError, 480101, "phone encryption failed")
	ErrDecryptPhone    = New(http.StatusInternalServerError, 480102, "phone decryption failed")
	ErrPhoneKeyMissing = New(http.StatusInternalServerError, 480103, "phone encryption key not configured")
)
