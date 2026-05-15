// Package cache — upload_cache.go encapsulates the Redis state behind the
// image-upload flow:
//
//	upload:nonce:{nonce} → JSON record bound at presign time. The callback
//	                       consumes it with GETDEL (Redis 6.2+) — an atomic
//	                       read-and-delete that guarantees a nonce can be
//	                       used exactly once. Replays after consumption,
//	                       and replays after TTL expiry, both surface as
//	                       ErrCacheNotFound.
//
// The nonce is the security primitive: it ties one /upload/callback request
// to one /upload/presign request, scoped to one user and one object_key.
// Without it, anyone who could guess an object_key could attach an OSS URL
// to their own profile by posting a callback directly.
//
// Added in Step 9 (image upload module).
package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// UploadCache wraps a Redis client with upload-module-specific operations.
// The cache layer does NOT own the rdb — same convention as UserCache.
type UploadCache struct {
	rdb *redis.Client
}

// NewUploadCache constructs an UploadCache.
func NewUploadCache(rdb *redis.Client) *UploadCache {
	return &UploadCache{rdb: rdb}
}

// ── Key builder ──────────────────────────────────────────────────────────────

func keyUploadNonce(nonce string) string { return "upload:nonce:" + nonce }

// ── NonceRecord — the value stored at upload:nonce:{nonce} ──────────────────

// NonceRecord is the post-presign state needed to authorise a future
// /upload/callback. Everything the callback needs to identify what was
// uploaded must come from here — NEVER trust the client to re-send fields.
type NonceRecord struct {
	UserID      int64  `json:"user_id"`
	ObjectKey   string `json:"object_key"`
	PublicURL   string `json:"public_url"`
	Purpose     string `json:"purpose"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
}

// ── Operations ──────────────────────────────────────────────────────────────

// SaveNonce persists the record with the configured TTL.
//
// Overwrite semantics: if (improbably) the same UUID nonce is generated
// twice, the second SET wins. With 128 bits of entropy from crypto/rand
// (see service.generateNonce), pairwise collision probability is 2^-128;
// documenting the SETEX behaviour matters more than guarding against it.
func (c *UploadCache) SaveNonce(ctx context.Context, nonce string, rec NonceRecord, ttl time.Duration) error {
	payload, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("marshal nonce record: %w", err)
	}
	if err := c.rdb.Set(ctx, keyUploadNonce(nonce), payload, ttl).Err(); err != nil {
		return fmt.Errorf("redis SET upload:nonce: %w", err)
	}
	return nil
}

// ConsumeNonce atomically reads and deletes the record. Returns
// ErrCacheNotFound when the key is absent (never set, already consumed,
// or TTL expired).
//
// GETDEL is a Redis 6.2+ primitive. The go-redis v9 client surfaces it as
// rdb.GetDel(...). It replaces the prior "GET then DEL" two-call pattern,
// closing the race where two simultaneous callers could each GET the same
// record before either could DEL it.
func (c *UploadCache) ConsumeNonce(ctx context.Context, nonce string) (*NonceRecord, error) {
	payload, err := c.rdb.GetDel(ctx, keyUploadNonce(nonce)).Bytes()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return nil, ErrCacheNotFound
		}
		return nil, fmt.Errorf("redis GETDEL upload:nonce: %w", err)
	}
	var rec NonceRecord
	if err := json.Unmarshal(payload, &rec); err != nil {
		// Corrupt record — surface as not-found rather than 500. The caller
		// experience is the same: their nonce is no longer usable.
		return nil, ErrCacheNotFound
	}
	return &rec, nil
}
