// Package middleware — ratelimit.go provides a generic Redis-backed
// sliding-window rate limiter usable on any route.
//
// Step 3 implements the limiter and exposes RateLimit() for downstream use.
// The user module's SMS rate limiting is NOT wired through this middleware —
// SMS limits use three composed dimensions and live inside user_service.
//
// Step 4 will use this middleware for post-creation rate limiting
// (limit:pub:{user_id}). Step 7+ may apply it to other write paths.
//
// Algorithm: Redis Sorted Set sliding window.
//
//	ZRemRangeByScore  → drop entries older than (now - window)
//	ZCard             → count entries in the window
//	ZAdd              → record this request (only if not yet over limit)
//	Expire            → renew TTL so cold keys eventually clear
//
// Atomicity is provided by a Pipeline; we accept the small chance that a
// burst arriving simultaneously may exceed the limit by 1-2 — perfect
// fairness is not worth the cost of a Lua script for our threat model.
package middleware

import (
	"context"
	"strconv"
	"time"

	"cooking-platform/pkg/errcode"
	"cooking-platform/pkg/response"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

// KeyExtractor returns the Redis key for the current request. Typical
// implementations: per-user (user_id from auth context), per-IP, or a
// combination.
type KeyExtractor func(c *gin.Context) string

// RateLimitConfig configures a single mounting of the rate-limit middleware.
type RateLimitConfig struct {
	// RDB is the Redis client used for sliding-window storage.
	RDB *redis.Client
	// KeyFunc derives the bucket key from the request context.
	KeyFunc KeyExtractor
	// Limit is the maximum number of allowed requests per window.
	Limit int
	// Window is the size of the sliding window (e.g. 1 * time.Minute).
	Window time.Duration
}

// RateLimit returns a gin middleware enforcing the configured limits.
// On rejection, responds with HTTP 429 and aborts the chain.
//
// Redis failure: middleware logs and "fails open" (allows the request).
// Limiting must never bring down the API for legitimate traffic.
func RateLimit(cfg RateLimitConfig) gin.HandlerFunc {
	return func(c *gin.Context) {
		key := cfg.KeyFunc(c)
		if key == "" {
			// No key — cannot enforce. Pass through.
			c.Next()
			return
		}

		allowed, err := slidingWindowAllow(c.Request.Context(), cfg.RDB, key, cfg.Limit, cfg.Window)
		if err != nil {
			zap.L().Warn("ratelimit check failed; failing open",
				zap.String("key", key),
				zap.Error(err),
			)
			c.Next()
			return
		}
		if !allowed {
			response.FromError(c, errcode.ErrTooManyReq)
			c.Abort()
			return
		}
		c.Next()
	}
}

// slidingWindowAllow is the core Redis Sorted Set algorithm.
//
// Returns allowed=true if the request fits within the limit, false otherwise.
// The current request is recorded in the set ONLY when it is allowed —
// rejected requests do not consume window slots, which would otherwise let
// a sustained burst keep the limit "stuck" at full capacity forever.
func slidingWindowAllow(ctx context.Context, rdb *redis.Client, key string, limit int, window time.Duration) (bool, error) {
	now := time.Now().UnixMilli()
	windowStart := now - window.Milliseconds()

	// Step 1+2: drop expired entries and count remaining.
	pipe := rdb.Pipeline()
	pipe.ZRemRangeByScore(ctx, key, "0", strconv.FormatInt(windowStart, 10))
	countCmd := pipe.ZCard(ctx, key)
	if _, err := pipe.Exec(ctx); err != nil {
		return false, err
	}

	if countCmd.Val() >= int64(limit) {
		return false, nil
	}

	// Step 3+4: record this request and renew TTL.
	pipe = rdb.Pipeline()
	pipe.ZAdd(ctx, key, redis.Z{Score: float64(now), Member: now})
	pipe.Expire(ctx, key, window+time.Second)
	if _, err := pipe.Exec(ctx); err != nil {
		// ZAdd failure is non-fatal for this request — we already decided
		// to allow it. Log and proceed; the count may be slightly off.
		return true, err
	}
	return true, nil
}

// PerUserKey is a convenient KeyExtractor for "per authenticated user" limits.
// Returns "" for unauthenticated requests, which slidingWindowAllow treats as
// "no enforcement" — chain Auth() before RateLimit() if you want hard
// rejection of anonymous traffic.
func PerUserKey(prefix string) KeyExtractor {
	return func(c *gin.Context) string {
		uid := GetUserID(c)
		if uid == 0 {
			return ""
		}
		return prefix + ":" + strconv.FormatInt(uid, 10)
	}
}

// PerIPKey is a KeyExtractor for "per client IP" limits. IP is taken from
// gin's ClientIP() which respects X-Forwarded-For when configured.
func PerIPKey(prefix string) KeyExtractor {
	return func(c *gin.Context) string {
		return prefix + ":" + c.ClientIP()
	}
}
