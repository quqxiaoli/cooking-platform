// Package middleware — auth.go validates JWT bearer tokens on protected
// routes and injects the authenticated user_id and jti into the gin context.
//
// Downstream handlers retrieve user_id via:
//
//	uid := middleware.GetUserID(c)
//
// On failure, the middleware responds with the appropriate error and aborts
// the chain — the protected handler never runs.
//
// Step 3 (user module).
package middleware

import (
	"context"
	"errors"

	"cooking-platform/pkg/errcode"
	"cooking-platform/pkg/response"

	"github.com/gin-gonic/gin"
)

// TokenVerifier validates access tokens and returns the associated uid and jti.
//
// Defined here so middleware does not import the service package.
// *service.UserService satisfies this interface without any changes.
type TokenVerifier interface {
	VerifyAccessToken(ctx context.Context, token string) (uid int64, jti string, err error)
}

// Context keys for authenticated request data. Strings are unexported to
// prevent accidental collisions with handler-set keys.
const (
	ctxKeyUserID = "auth_user_id"
	ctxKeyJTI    = "auth_jti"
)

// Auth returns a middleware that requires a valid Bearer token.
//
// The middleware:
//  1. Extracts the token from the Authorization header
//  2. Calls v.VerifyAccessToken to validate signature, blacklist, ban
//  3. Stores user_id and jti in the gin context for downstream use
func Auth(v TokenVerifier) gin.HandlerFunc {
	return func(c *gin.Context) {
		token, err := extractBearerToken(c.GetHeader("Authorization"))
		if err != nil {
			response.Unauthorized(c, errcode.ErrUnauthorized)
			c.Abort()
			return
		}

		uid, jti, err := v.VerifyAccessToken(c.Request.Context(), token)
		if err != nil {
			var appErr *errcode.AppError
			if errors.As(err, &appErr) {
				response.FromError(c, appErr)
			} else {
				response.Unauthorized(c, errcode.ErrUnauthorized)
			}
			c.Abort()
			return
		}

		c.Set(ctxKeyUserID, uid)
		c.Set(ctxKeyJTI, jti)
		c.Next()
	}
}

// OptionalAuth returns a middleware that populates the authenticated user_id
// when a valid Bearer token is present, but never aborts the request. Used
// on otherwise-public routes (e.g. /feed, /users/:id, /search) that want to
// enrich the response with viewer-specific fields (is_following, liked_by_me)
// without forcing login.
//
// Missing header, malformed scheme, expired / blacklisted / invalid tokens
// all degrade silently — the request proceeds as anonymous (GetUserID
// returns 0). This keeps the public-facing read path tolerant of clients
// that drift their refresh window or forget to attach a header.
func OptionalAuth(v TokenVerifier) gin.HandlerFunc {
	return func(c *gin.Context) {
		token, err := extractBearerToken(c.GetHeader("Authorization"))
		if err != nil {
			c.Next()
			return
		}
		uid, jti, err := v.VerifyAccessToken(c.Request.Context(), token)
		if err != nil {
			c.Next()
			return
		}
		c.Set(ctxKeyUserID, uid)
		c.Set(ctxKeyJTI, jti)
		c.Next()
	}
}

// GetUserID returns the authenticated user_id from the context, or 0 if the
// request is unauthenticated. Handlers protected by Auth() can rely on the
// value being non-zero; public handlers using this for optional-auth must
// explicitly check for zero.
func GetUserID(c *gin.Context) int64 {
	v, ok := c.Get(ctxKeyUserID)
	if !ok {
		return 0
	}
	uid, ok := v.(int64)
	if !ok {
		return 0
	}
	return uid
}

// GetJTI returns the access token's JTI for the current request, or empty
// string if absent. The logout handler uses this to blacklist the in-flight
// token.
func GetJTI(c *gin.Context) string {
	v, ok := c.Get(ctxKeyJTI)
	if !ok {
		return ""
	}
	jti, _ := v.(string)
	return jti
}

// extractBearerToken parses an Authorization header of the form "Bearer <token>".
// Trailing/leading whitespace is tolerated; case-insensitive scheme match.
func extractBearerToken(header string) (string, error) {
	const prefix = "Bearer "
	if len(header) <= len(prefix) {
		return "", errors.New("missing bearer token")
	}
	// Case-insensitive prefix match: some clients send "bearer".
	if !equalFold(header[:len(prefix)], prefix) {
		return "", errors.New("invalid auth scheme")
	}
	return header[len(prefix):], nil
}

// equalFold is a tiny case-insensitive ASCII comparator. Avoids pulling in
// strings just for this — middleware is on the hot path.
func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if ca >= 'A' && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if cb >= 'A' && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}
