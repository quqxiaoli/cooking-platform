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
	ErrInvalidParams = New(http.StatusBadRequest, 400001, "invalid request parameters")
	ErrUnauthorized  = New(http.StatusUnauthorized, 401001, "unauthorized")
	ErrTokenExpired  = New(http.StatusUnauthorized, 401002, "token expired")
	ErrTokenInvalid  = New(http.StatusUnauthorized, 401003, "token invalid")
	ErrForbidden     = New(http.StatusForbidden, 403001, "forbidden")
	ErrNotFound      = New(http.StatusNotFound, 404001, "resource not found")
	ErrTooManyReq    = New(http.StatusTooManyRequests, 429001, "too many requests")
)

// ── 5xx Server Errors ───────────────────────────────────────────────────────
var (
	ErrServer         = New(http.StatusInternalServerError, 500001, "internal server error")
	ErrDatabase       = New(http.StatusInternalServerError, 500002, "database error")
	ErrCacheError     = New(http.StatusInternalServerError, 500003, "cache error")
	ErrServiceUnavail = New(http.StatusServiceUnavailable, 503001, "service unavailable")
)

// ── User Module (4xx, code segment 41xxxx) ──────────────────────────────────
// [Step 3] Defined alongside user registration / login / profile.
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

// ── Post Module (4xx, code segment 412xxx) ─────────────────────────────────
// [Step 4] Defined alongside post creation / feed / detail / author page.
//
// Numbering convention: user module owns 410xxx, post module owns 412xxx.
// 411xxx is reserved for a future user-extension area (e.g. account
// recovery, device management) so we don't fragment numbering when those
// land. Numbering scheme matches errcode.go's header: XYYZZZ.
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

// ── Search Module (4xx, code segment 450xxx) ───────────────────────────────
// [Step 7] Defined alongside full-text keyword search.
//
// Numbering note: this module takes the full 450xxx block. Steps 3-4 used a
// tighter 41Zxxx layout (user=410, post=412, 411 reserved) which crowded
// modules together. From Step 7 on, each new module owns a clean 4MM xxx
// block — search=450, follow=440, upload=460, audit=470 — so segments never
// collide and the allocation is obvious. The 41x↔45x width inconsistency
// with the earlier modules is a known cosmetic debt, to be reconciled in
// Step 12's error-code consolidation. Numbering scheme stays XYYZZZ.
var (
	ErrSearchKeywordEmpty  = New(http.StatusBadRequest, 450101, "search keyword cannot be empty")
	ErrSearchCursorInvalid = New(http.StatusBadRequest, 450102, "invalid search cursor")
)

// ── Follow Module (4xx, code segment 440xxx) ───────────────────────────────
// [Step 8] Defined alongside follow / unfollow / follower-list / following-list.
//
// Reused codes (NOT redefined here):
//   - target user does not exist → 410108 ErrUserNotFound
//   - caller not logged in       → 401001 ErrUnauthorized (middleware.Auth)
var (
	ErrCannotFollowSelf    = New(http.StatusBadRequest, 440101, "cannot follow yourself")
	ErrFollowLimitExceeded = New(http.StatusBadRequest, 440102, "following limit reached (max 3000)")
	ErrFollowNotFound      = New(http.StatusNotFound, 440103, "follow relationship not found")
	ErrFollowCursorInvalid = New(http.StatusBadRequest, 440104, "invalid follow list cursor")
)

// ── Upload Module (4xx, code segment 460xxx) ───────────────────────────────
// [Step 9] Defined alongside OSS presign / callback and the image fields
// they unlock on user profiles and posts.
//
// Numbering: upload module owns the full 460xxx block, per the "each new
// module owns a clean 4MMxxx block" convention adopted from Step 7.
//
// Reused codes (NOT redefined here):
//   - caller not logged in → 401001 ErrUnauthorized (middleware.Auth)
//   - global rate limit    → 429001 ErrTooManyReq   (middleware.RateLimit)
//   - avatar URL parse fail on UpdateProfile path  → 410112 ErrAvatarURLInvalid
//     is reused when the supplied
//     string is otherwise malformed;
//     460105 is used when the URL
//     is parseable but off-host.
var (
	ErrUploadFileType        = New(http.StatusBadRequest, 460101, "unsupported file type")
	ErrUploadFileTooLarge    = New(http.StatusBadRequest, 460102, "file exceeds size limit")
	ErrUploadPresignFailed   = New(http.StatusInternalServerError, 460103, "failed to generate presigned url")
	ErrUploadCallbackInvalid = New(http.StatusBadRequest, 460104, "invalid callback nonce")
	ErrUploadURLNotAllowed   = New(http.StatusBadRequest, 460105, "url not in oss whitelist")
	ErrPostStepsInvalid      = New(http.StatusBadRequest, 460106, "post steps invalid")
)

// ── Audit Module (5xx, code segment 470xxx) ────────────────────────────────
// [Step 10] Internal errors emitted by AuditConsumer. Never returned to HTTP
// callers — audit runs fully async. Codes exist for structured log tagging
// so Prometheus / Grafana can alert on audit pipeline failures.
//
// Reused codes (NOT redefined here):
//   - caller not logged in → 401001 ErrUnauthorized
//   - resource not found   → 404001 ErrNotFound
var (
	ErrAuditAPIFailed   = New(http.StatusInternalServerError, 470101, "content safety API call failed")
	ErrAuditWriteFailed = New(http.StatusInternalServerError, 470102, "audit result write failed")
)

// ── Encryption Module (5xx, code segment 480xxx) ───────────────────────────
// [Step 11] AES-GCM phone field-level encryption errors. Never returned to
// HTTP callers — encryption is transparent infrastructure. Codes exist for
// structured log tagging (Prometheus alert on key misconfiguration).
var (
	ErrEncryptPhone = New(http.StatusInternalServerError, 480101, "phone encryption failed")
	ErrDecryptPhone = New(http.StatusInternalServerError, 480102, "phone decryption failed")
)
