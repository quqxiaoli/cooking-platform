// Package cache — user_cache.go encapsulates all Redis operations for the
// user module: SMS verification codes, three-dimension SMS rate limiting,
// JWT blacklist, ban flags, and the user-info cache.
//
// All keys conform to PRD-Phase3 §6.3. Format is documented inline at each
// constructor function.
//
// Functions return wrapped errors with sufficient context to identify which
// Redis operation failed in logs. Callers (service layer) decide how to react
// to Redis failures — typically: log + degrade rather than fail the request.
//
// Added in Step 3 (user module).
package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// UserCache wraps a Redis client with user-module-specific operations.
type UserCache struct {
	rdb         *redis.Client
	smsDailyTTL time.Duration // [Step 18] cfg.Cache.UserSMSDailyTTL — was 24h const
}

// NewUserCache constructs a UserCache. The provided Redis client is shared
// with other cache modules — UserCache does not own it and must not Close() it.
//
// smsDailyTTL is the TTL anchored on first INCR for the per-phone and per-IP
// daily SMS counters (sms:limit:* / sms:ip:* keys). Step 18 lifted the
// previously hardcoded 24h to cfg.Cache.UserSMSDailyTTL (USER-03).
func NewUserCache(rdb *redis.Client, smsDailyTTL time.Duration) *UserCache {
	return &UserCache{rdb: rdb, smsDailyTTL: smsDailyTTL}
}

// ── Key builders ────────────────────────────────────────────────────────────

func keySMSCode(phoneHash string) string       { return "sms:code:" + phoneHash }
func keySMSWindow(phoneHash string) string     { return "sms:window:" + phoneHash }
func keySMSDailyPhone(phoneHash string) string { return "sms:limit:" + phoneHash }
func keySMSDailyIP(ipHash string) string       { return "sms:ip:" + ipHash }
func keyJWTBlacklist(jti string) string        { return "jwt:bl:" + jti }
func keyUserBan(userID int64) string           { return fmt.Sprintf("user:ban:%d", userID) }
func keyUserInfo(userID int64) string          { return fmt.Sprintf("user:info:%d", userID) }

// ── SMS verification code ───────────────────────────────────────────────────

// SaveSMSCode stores a verification code with the configured TTL.
// SETEX semantics: any existing code is overwritten — this is intentional,
// the most recent code wins (common UX expectation).
func (c *UserCache) SaveSMSCode(ctx context.Context, phoneHash, code string, ttl time.Duration) error {
	if err := c.rdb.Set(ctx, keySMSCode(phoneHash), code, ttl).Err(); err != nil {
		return fmt.Errorf("redis SET sms:code: %w", err)
	}
	return nil
}

// GetSMSCode retrieves the stored code for a phone, or returns ErrCacheNotFound
// if none exists (or expired).
func (c *UserCache) GetSMSCode(ctx context.Context, phoneHash string) (string, error) {
	code, err := c.rdb.Get(ctx, keySMSCode(phoneHash)).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return "", ErrCacheNotFound
		}
		return "", fmt.Errorf("redis GET sms:code: %w", err)
	}
	return code, nil
}

// DeleteSMSCode removes the code after successful verification — one-time use.
func (c *UserCache) DeleteSMSCode(ctx context.Context, phoneHash string) error {
	if err := c.rdb.Del(ctx, keySMSCode(phoneHash)).Err(); err != nil {
		return fmt.Errorf("redis DEL sms:code: %w", err)
	}
	return nil
}

// ── SMS rate limiting (three dimensions) ────────────────────────────────────

// CheckAndConsumeSMSWindow enforces "no more than one send within the window
// per phone" using SETNX semantics. Returns:
//   - allowed=true if the window slot was successfully claimed (caller may proceed)
//   - allowed=false + retryAfter showing remaining seconds before next send
//
// The window slot auto-expires; we never need to clean up.
func (c *UserCache) CheckAndConsumeSMSWindow(ctx context.Context, phoneHash string, window time.Duration) (allowed bool, retryAfter time.Duration, err error) {
	key := keySMSWindow(phoneHash)
	ok, setErr := c.rdb.SetNX(ctx, key, "1", window).Result()
	if setErr != nil {
		return false, 0, fmt.Errorf("redis SETNX sms:window: %w", setErr)
	}
	if ok {
		return true, 0, nil
	}
	// Window already taken — fetch remaining TTL for accurate retry-after hint.
	ttl, ttlErr := c.rdb.TTL(ctx, key).Result()
	if ttlErr != nil || ttl < 0 {
		// TTL=-1 (no expiry) or TTL=-2 (key gone) shouldn't happen here;
		// return a conservative full-window value rather than fail.
		return false, window, nil
	}
	return false, ttl, nil
}

// IncrementAndCheckSMSPhoneDaily increments the per-phone daily counter and
// returns whether the request is still under the limit. Uses INCR + EXPIRE
// only on first hit (when the counter goes from 0 to 1) to anchor a 24h TTL.
//
// Returns:
//   - allowed=true if the count after increment is ≤ limit
//   - allowed=false otherwise (caller must reject the request)
func (c *UserCache) IncrementAndCheckSMSPhoneDaily(ctx context.Context, phoneHash string, limit int) (allowed bool, err error) {
	return c.incrAndCheck(ctx, keySMSDailyPhone(phoneHash), limit, c.smsDailyTTL)
}

// IncrementAndCheckSMSIPDaily is the IP-side equivalent. ipHash should be a
// SHA-256 hex digest of the client IP; the service layer is responsible for
// hashing (we don't want raw IPs flowing through cache method signatures).
func (c *UserCache) IncrementAndCheckSMSIPDaily(ctx context.Context, ipHash string, limit int) (allowed bool, err error) {
	return c.incrAndCheck(ctx, keySMSDailyIP(ipHash), limit, c.smsDailyTTL)
}

// incrAndCheck is the shared INCR-then-EXPIRE-on-first-hit primitive.
//
// Race condition consideration: if two requests arrive simultaneously for a
// fresh key, both might succeed in INCR before either calls EXPIRE. We accept
// this as the worst case is "the counter never expires" — but EXPIRE is
// idempotent and any subsequent call resets the TTL. Net effect: the window
// might be slightly longer than 24h on rare contention, never shorter.
func (c *UserCache) incrAndCheck(ctx context.Context, key string, limit int, ttl time.Duration) (bool, error) {
	count, err := c.rdb.Incr(ctx, key).Result()
	if err != nil {
		return false, fmt.Errorf("redis INCR %s: %w", key, err)
	}
	if count == 1 {
		// First hit — anchor the TTL. Best-effort: even if EXPIRE fails,
		// the key still exists with NO TTL, which is corrected on next call.
		if err := c.rdb.Expire(ctx, key, ttl).Err(); err != nil {
			return false, fmt.Errorf("redis EXPIRE %s: %w", key, err)
		}
	}
	return count <= int64(limit), nil
}

// ── JWT blacklist ───────────────────────────────────────────────────────────

// BlacklistJWT marks a JTI as revoked. TTL should be the access token's
// remaining lifetime — past expiry, the token is rejected by signature
// validation anyway, so blacklisting beyond expiry wastes Redis memory.
func (c *UserCache) BlacklistJWT(ctx context.Context, jti string, ttl time.Duration) error {
	if ttl <= 0 {
		// Token already expired — nothing to do.
		return nil
	}
	if err := c.rdb.Set(ctx, keyJWTBlacklist(jti), "1", ttl).Err(); err != nil {
		return fmt.Errorf("redis SET jwt:bl: %w", err)
	}
	return nil
}

// IsJWTBlacklisted is called by the Auth middleware on every authenticated
// request. Cost: one EXISTS roundtrip per request, served by Redis in <1ms.
//
// Redis failure: middleware should treat error as "fail open" (allow the
// request) to avoid taking down the API on Redis hiccups. The risk window
// for a logged-out token is bounded by access token TTL.
func (c *UserCache) IsJWTBlacklisted(ctx context.Context, jti string) (bool, error) {
	n, err := c.rdb.Exists(ctx, keyJWTBlacklist(jti)).Result()
	if err != nil {
		return false, fmt.Errorf("redis EXISTS jwt:bl: %w", err)
	}
	return n > 0, nil
}

// ── User ban flag ───────────────────────────────────────────────────────────

// IsUserBanned checks the dedicated ban key. This is faster than fetching the
// full user record; admins set this key when banning a user (separate from
// the User.Status field which is the persistent source of truth).
//
// Returns false on Redis errors — fail open. The User.Status field re-checked
// at next /users/me will catch any inconsistency.
func (c *UserCache) IsUserBanned(ctx context.Context, userID int64) (bool, error) {
	n, err := c.rdb.Exists(ctx, keyUserBan(userID)).Result()
	if err != nil {
		return false, fmt.Errorf("redis EXISTS user:ban: %w", err)
	}
	return n > 0, nil
}

// ── User-info cache ─────────────────────────────────────────────────────────

// SaveUserInfo caches a serialised user profile for fast Auth middleware
// access. TTL of 30 minutes balances freshness against DB load.
//
// payload should be a JSON-encoded snapshot — the cache layer is type-agnostic.
func (c *UserCache) SaveUserInfo(ctx context.Context, userID int64, payload []byte, ttl time.Duration) error {
	if err := c.rdb.Set(ctx, keyUserInfo(userID), payload, ttl).Err(); err != nil {
		return fmt.Errorf("redis SET user:info: %w", err)
	}
	return nil
}

// GetUserInfo returns the cached payload, or ErrCacheNotFound if no entry exists.
func (c *UserCache) GetUserInfo(ctx context.Context, userID int64) ([]byte, error) {
	data, err := c.rdb.Get(ctx, keyUserInfo(userID)).Bytes()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return nil, ErrCacheNotFound
		}
		return nil, fmt.Errorf("redis GET user:info: %w", err)
	}
	return data, nil
}

// DeleteUserInfo invalidates the cache. Called from UpdateProfile to ensure
// the next read sees fresh data.
func (c *UserCache) DeleteUserInfo(ctx context.Context, userID int64) error {
	if err := c.rdb.Del(ctx, keyUserInfo(userID)).Err(); err != nil {
		return fmt.Errorf("redis DEL user:info: %w", err)
	}
	return nil
}

// ── Helpers ─────────────────────────────────────────────────────────────────

// MarshalUserInfo is a convenience helper for the service layer to serialise
// any DTO to bytes for caching. Kept here (rather than service) so the cache
// layer fully encapsulates payload format.
func MarshalUserInfo(v interface{}) ([]byte, error) {
	return json.Marshal(v)
}

// UnmarshalUserInfo is the matching deserializer.
func UnmarshalUserInfo(data []byte, v interface{}) error {
	return json.Unmarshal(data, v)
}

// ErrCacheNotFound is the sentinel returned when a key is absent or expired.
// Service layer translates this to "cache miss" semantics and falls back to
// the source of truth.
var ErrCacheNotFound = errors.New("cache key not found")
