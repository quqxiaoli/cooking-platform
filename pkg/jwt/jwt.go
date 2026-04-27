// Package jwt provides JWT token generation, parsing, and validation for the user
// authentication module.
//
// Algorithm: HS256 (symmetric, single-service simplicity).
// Claims: standard RegisteredClaims (exp/iat/nbf) plus uid (int64) and jti (UUIDv4).
//
// Two token types share the same struct but differ in TTL and intent:
//   - AccessToken: short-lived (cfg.JWT.AccessTokenTTL, default 2h), sent in
//     Authorization: Bearer header on every API call.
//   - RefreshToken: long-lived (cfg.JWT.RefreshTokenTTL, default 168h), used
//     only to obtain a new access token via /api/v1/auth/refresh.
//
// JTI (JWT ID) is a UUIDv4 written into Redis on logout (jwt:bl:{jti}) so that
// access tokens can be revoked before their natural expiry.
//
// Added in Step 3 (user module).
package jwt

import (
	"errors"
	"fmt"
	"time"

	"cooking-platform/pkg/config"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// Claims is the JWT payload carried by both access and refresh tokens.
//
// Json tags use short names ("uid", "jti") to keep tokens compact —
// every byte counts when tokens are sent on every request.
type Claims struct {
	UserID int64  `json:"uid"`
	JTI    string `json:"jti"`
	jwt.RegisteredClaims
}

// TokenType distinguishes access vs refresh tokens. Stored in the standard
// "typ" field of RegisteredClaims is technically possible but mixing concerns;
// we keep type out-of-band (caller knows which TTL it wants).
type TokenType int

const (
	AccessToken TokenType = iota
	RefreshToken
)

// Manager is the package's main entry point. Construct one at startup with
// NewManager(cfg.JWT) and inject it into the service layer.
type Manager struct {
	secret          []byte
	accessTokenTTL  time.Duration
	refreshTokenTTL time.Duration
}

// NewManager constructs a JWT Manager from configuration.
//
// The secret length is enforced at config load time (≥ 32 chars) so we don't
// double-validate here.
func NewManager(cfg config.JWTConfig) *Manager {
	return &Manager{
		secret:          []byte(cfg.Secret),
		accessTokenTTL:  cfg.AccessTokenTTL,
		refreshTokenTTL: cfg.RefreshTokenTTL,
	}
}

// IssueAccessToken creates a signed access token for the given user and returns
// the encoded string, the JTI (so the caller can invalidate it later), and the
// expiry time (so callers can compute Redis blacklist TTL).
func (m *Manager) IssueAccessToken(userID int64) (token string, jti string, expiresAt time.Time, err error) {
	return m.issue(userID, m.accessTokenTTL)
}

// IssueRefreshToken creates a signed refresh token. Refresh tokens are stored
// only client-side; the server does not track them. Compromise of a refresh
// token therefore allows full account takeover until expiry — keep them safe
// (httpOnly cookie or secure storage on mobile).
func (m *Manager) IssueRefreshToken(userID int64) (token string, jti string, expiresAt time.Time, err error) {
	return m.issue(userID, m.refreshTokenTTL)
}

// AccessTokenTTL exposes the configured access token lifetime so callers
// (e.g. logout handler) can compute the precise remaining TTL when blacklisting.
func (m *Manager) AccessTokenTTL() time.Duration {
	return m.accessTokenTTL
}

// RefreshTokenTTL exposes the configured refresh token lifetime.
func (m *Manager) RefreshTokenTTL() time.Duration {
	return m.refreshTokenTTL
}

// issue is the shared implementation for both token types.
func (m *Manager) issue(userID int64, ttl time.Duration) (string, string, time.Time, error) {
	now := time.Now()
	expiresAt := now.Add(ttl)
	jti := uuid.NewString()

	claims := Claims{
		UserID: userID,
		JTI:    jti,
		RegisteredClaims: jwt.RegisteredClaims{
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(expiresAt),
			Issuer:    "cooking-platform",
		},
	}

	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := tok.SignedString(m.secret)
	if err != nil {
		return "", "", time.Time{}, fmt.Errorf("sign token: %w", err)
	}
	return signed, jti, expiresAt, nil
}

// Common parse errors. Callers (Auth middleware, refresh handler) should map
// these to HTTP error responses via errcode.ErrTokenExpired / ErrTokenInvalid.
var (
	ErrTokenExpired = errors.New("token expired")
	ErrTokenInvalid = errors.New("token invalid")
)

// Parse validates a token string and returns its claims if signature and
// expiry are valid. Returns ErrTokenExpired or ErrTokenInvalid on failure.
//
// Note: this method does NOT check the JWT blacklist. The Auth middleware
// must additionally check Redis (jwt:bl:{jti}) to honour logout.
func (m *Manager) Parse(tokenString string) (*Claims, error) {
	claims := &Claims{}
	tok, err := jwt.ParseWithClaims(tokenString, claims, func(t *jwt.Token) (interface{}, error) {
		// Reject any algorithm except the one we sign with — protects against
		// the classic "alg: none" / algorithm confusion attack.
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return m.secret, nil
	})

	if err != nil {
		// jwt/v5 wraps multiple error kinds; classify them for the caller.
		if errors.Is(err, jwt.ErrTokenExpired) {
			return nil, ErrTokenExpired
		}
		return nil, ErrTokenInvalid
	}

	if !tok.Valid {
		return nil, ErrTokenInvalid
	}
	if claims.UserID <= 0 || claims.JTI == "" {
		return nil, ErrTokenInvalid
	}

	return claims, nil
}
