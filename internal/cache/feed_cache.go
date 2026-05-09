// Package cache — feed_cache.go encapsulates Redis operations for the Feed
// pipeline: the global feed version counter, per-page feed payload cache,
// and PV deduplication.
//
// All keys conform to PRD-Phase3 §6.3:
//
//	feed:ver                                    String, permanent
//	feed:s{scene}:v{ver}:c{cursorMs}            String(JSON), TTL 300s
//	pv:dup:{post_id}:u{user_id}                 String, TTL 3600s (logged-in viewers)
//	pv:dup:{post_id}:i{ip_hash}                 String, TTL 3600s (anonymous viewers)
//
// ── Why version-keyed Feed caching, not invalidation-on-write? ──────────────
//
// The naive approach to Feed caching is "on every new post, DEL all feed
// keys". Three problems:
//
//  1. Redis SCAN to find all `feed:*` keys is O(N) over the whole keyspace
//     and blocks other commands for the duration. With 100k+ feed keys
//     it's a self-inflicted DoS.
//  2. KEYS is even worse — same scan but synchronous.
//  3. Pattern-DEL via Lua hits Redis cluster sharding limits (different
//     hash slots).
//
// Version-keyed caching sidesteps all of that:
//
//   - feed:ver is a single integer counter. Every write that affects Feed
//     visibility (new post, audit pass, audit reject, soft delete) calls
//     INCR feed:ver. ZERO scans, O(1) write.
//   - Read path: GET feed:ver → resolve current ver → GET feed:s{scene}:v{ver}:c{cursorMs}.
//     Stale entries from older versions are NEVER read because the key
//     name embeds the ver — they simply expire after their 300s TTL.
//   - Memory cost: roughly 5 minutes of stale cache lingering = a few MB.
//     A trivial price for the consistency simplicity.
//
// ── Why the PV dedup key splits user vs IP buckets ──────────────────────────
//
// Logged-in users get a single canonical key (uid). Anonymous viewers
// share an IP-hash bucket — multiple devices behind one NAT count as one
// view per hour, which is fine: PV is a popularity signal, not an attribution
// system. Mixing the two would let an attacker force-count anonymous IPs
// against a logged-in user's quota.
//
// Future improvements:
//   - When MAU > 100k, evaluate Bloom Filter for PV dedup (PRD §6.6 keys
//     `bloom:like:{post_id}` design also applies to PV). Bloom trades exact
//     accuracy for ~10x memory savings on hot posts.
//   - Replace MarkPVSeen's GET-then-SETEX with SET NX EX (atomic). Today's
//     two-step has a tiny race window where two concurrent first-views
//     both count as first; harmless but inelegant. Migrate when we touch
//     this for a different reason.
//   - Compress feed cache payloads with snappy/gzip for >5KB pages once
//     payload size becomes an OOM concern. Today's sizes are <2KB per page.
//
// Added in Step 4 (content module).
package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// FeedCache exposes Feed-related Redis operations.
type FeedCache struct {
	rdb *redis.Client
}

// NewFeedCache constructs a FeedCache.
func NewFeedCache(rdb *redis.Client) *FeedCache {
	return &FeedCache{rdb: rdb}
}

// ── Key constants & TTLs ────────────────────────────────────────────────────

const (
	// feedVersionKey is the global Feed cache version. INCR on any write
	// that affects what Feed should show (post create / audit transition /
	// soft delete).
	feedVersionKey = "feed:ver"

	// FeedCacheTTL is the lifetime of a single cached feed page. 5 minutes
	// is a balance: long enough to absorb a thundering herd on a hot scene
	// tag, short enough that an unexpected stale page (e.g. across a
	// version bump that this read missed) self-heals quickly.
	FeedCacheTTL = 5 * time.Minute

	// PVDedupTTL is the dedup window for view counting: a user's repeat
	// view of the same post within this window does NOT increment view_count.
	// 1 hour matches PRD-Phase2 §6 F-I02 AC-2.
	PVDedupTTL = 1 * time.Hour
)

// ── Feed version ────────────────────────────────────────────────────────────

// GetFeedVersion returns the current Feed cache version.
//
// On the very first call after deployment / Redis flush the key doesn't
// exist; we treat that as version 0 and proceed (the first read will MISS
// and populate, the first write will INCR to 1). go-redis returns
// redis.Nil for missing keys; we map that to 0 explicitly.
func (c *FeedCache) GetFeedVersion(ctx context.Context) (int64, error) {
	v, err := c.rdb.Get(ctx, feedVersionKey).Int64()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return 0, nil
		}
		return 0, err
	}
	return v, nil
}

// BumpFeedVersion atomically increments and returns the new version.
//
// INCR auto-creates the key as 0 then increments to 1, so we don't need a
// separate "init" path. The new version is returned so callers can log it
// for observability ("post 12345 created → feed:ver = 7").
func (c *FeedCache) BumpFeedVersion(ctx context.Context) (int64, error) {
	return c.rdb.Incr(ctx, feedVersionKey).Result()
}

// ── Feed payload cache ──────────────────────────────────────────────────────

// feedCacheKey builds the deterministic key used for both reads and writes.
//
// scene == 0 represents "all scenes". cursorTime.IsZero() represents the
// first page (cursor=""). Both edge cases must produce stable strings so
// the read path's lookup matches the write path's insert.
func feedCacheKey(scene int8, version int64, cursorTime time.Time) string {
	cursorMs := int64(0)
	if !cursorTime.IsZero() {
		cursorMs = cursorTime.UnixMilli()
	}
	// %d for scene: 0 prints as "0", which is the all-scenes channel.
	return fmt.Sprintf("feed:s%d:v%d:c%d", scene, version, cursorMs)
}

// GetFeed returns the cached JSON payload for (scene, version, cursor) or
// (nil, nil) on a cache miss. Other Redis errors are returned as-is.
//
// Returning a nil-data + nil-error on miss (rather than a sentinel error)
// matches the user_cache idiom in this codebase and keeps callers shorter:
//
//	data, err := c.GetFeed(...)
//	if err != nil { return err }
//	if data == nil { /* miss → query DB */ }
func (c *FeedCache) GetFeed(ctx context.Context, scene int8, version int64, cursorTime time.Time) ([]byte, error) {
	data, err := c.rdb.Get(ctx, feedCacheKey(scene, version, cursorTime)).Bytes()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return nil, nil
		}
		return nil, err
	}
	return data, nil
}

// SetFeed writes the JSON payload with the standard FeedCacheTTL.
//
// We use SETEX semantics (Set with expiration) rather than SET + EXPIRE
// to keep the operation atomic — a crash between SET and EXPIRE would
// leak a permanent key, slowly filling Redis over months.
func (c *FeedCache) SetFeed(ctx context.Context, scene int8, version int64, cursorTime time.Time, data []byte, ttl time.Duration) error {
	return c.rdb.Set(ctx, feedCacheKey(scene, version, cursorTime), data, ttl).Err()
}

// ── PV deduplication ────────────────────────────────────────────────────────

// pvDedupKey builds the dedup key for either a logged-in user (uid > 0)
// or an anonymous viewer (uid == 0, ip used).
//
// Buckets are namespaced by `u` / `i` so a logged-in user with UID=42
// can't collide with an IP whose hash happens to start with "42…".
func pvDedupKey(postID int64, viewerID int64, ipHash string) string {
	if viewerID > 0 {
		return fmt.Sprintf("pv:dup:%d:u%d", postID, viewerID)
	}
	return fmt.Sprintf("pv:dup:%d:i%s", postID, ipHash)
}

// MarkPVSeen records that a viewer has seen this post within the dedup
// window. Returns firstView=true if this is the first view in the window
// (caller should publish a PVEvent), false otherwise.
//
// Implementation note: SET NX EX is atomic — exactly one concurrent caller
// will see firstView=true even if hundreds race in within the same
// millisecond. Compared to the GET-then-SET pattern, SET NX EX also
// halves the round-trip count.
//
// For anonymous viewers, viewerID must be 0 and ip must be a non-empty
// raw IP string (we hash here so the caller doesn't have to).
func (c *FeedCache) MarkPVSeen(ctx context.Context, postID, viewerID int64, ip string) (firstView bool, err error) {
	var ipHash string
	if viewerID == 0 {
		ipHash = hashIPForKey(ip)
	}
	key := pvDedupKey(postID, viewerID, ipHash)

	// NX = only set if not exists; EX = TTL in seconds. The reply is
	// "OK" on insertion, nil on collision — go-redis surfaces this as
	// (true, nil) / (false, nil) via SetNX().
	ok, err := c.rdb.SetNX(ctx, key, 1, PVDedupTTL).Result()
	if err != nil {
		return false, err
	}
	return ok, nil
}

// hashIPForKey produces the SHA-256 hex digest of an IP for use in dedup
// keys. Hashing avoids storing raw IPs in Redis (GDPR-aligned habit, same
// rationale as user_service's hashIP helper).
//
// Empty IP collapses to a sentinel hash so the key is still well-formed;
// in practice gin's c.ClientIP() never returns empty for real requests.
func hashIPForKey(ip string) string {
	if ip == "" {
		ip = "unknown"
	}
	sum := sha256.Sum256([]byte(ip))
	return hex.EncodeToString(sum[:])
}
