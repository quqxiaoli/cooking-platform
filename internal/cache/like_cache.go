// Package cache — like_cache.go encapsulates Redis operations for the like
// module: presence-set membership for idempotency, atomic counter for
// real-time UI feedback, and the per-user 24h rate-limit bucket.
//
// All keys conform to PRD-Phase3 §6.3:
//
//	like:set:{post_id}    SET<user_id>     TTL 7d (sliding)  — who has liked this post
//	like:cnt:{post_id}    String(int)      TTL 7d (sliding)  — current like count (real-time)
//	limit:like:{user_id}  ZSet(score=ms)   TTL = window      — sliding-window rate limiter
//
// ── Why a Redis SET, not a String "user X liked post Y" key ─────────────────
//
// Per-pair String keys (e.g. `liked:{post_id}:{user_id}=1`) would scale
// linearly with total likes ever issued, which is fine for storage but
// disastrous for two queries we actually need:
//
//   - SISMEMBER (called on every like / unlike to test idempotency) is O(1).
//     A per-pair String approach would also be O(1) for this single test,
//     but we'd lose the next one:
//   - SCARD / SMEMBERS (debugging, future "who liked this post" feature)
//     against a SET is one call. Against per-pair Strings it's a SCAN,
//     which is forbidden in our design (PRD §6.3 explicitly bans SCAN-based
//     invalidation).
//
// Memory: a SET of 10k user_ids costs ~600KB (Redis ziplist→hashtable transition
// at ~512 entries; after that ~60 bytes per int64 member). Acceptable for
// hot posts; cold posts trim themselves via the 7d sliding TTL.
//
// ── Why a SEPARATE String counter, not SCARD ────────────────────────────────
//
// Two reasons we don't compute count on-demand from SCARD:
//
//  1. SCARD is O(1), but only after the SET has been fully resident in
//     memory. For a SET evicted to disk-swap or cold-cache it can be slow.
//     A separate counter is *always* O(1), no exceptions.
//  2. The counter survives independently if the SET ever needs to be
//     trimmed (e.g. capping membership to top-N likers in a future
//     "hot-post degradation" mode). SCARD would then under-report.
//
// We accept the small inconsistency window between SADD-and-INCR (and
// SREM-and-DECR): two non-atomic Redis ops mean a millisecond-level race
// can produce SET-size != counter. LikeConsumer's eventual write to MySQL
// is the source of truth; the Redis count is best-effort UI sugar.
//
// ── Why TTL 7d sliding, not permanent ───────────────────────────────────────
//
// "Permanent" Redis keys for every post ever created would grow unbounded.
// 7-day sliding (refreshed on every read/write touching the key) means:
//   - Hot posts (read/written frequently) keep their like state in Redis.
//   - Cold posts age out → next like-status query falls back to MySQL
//     (LikeService recomputes from the likes table; not implemented yet
//     because GET /like is rare, but the path is open).
//
// 7d is empirically wide enough that re-engagement waves (e.g. weekly
// digest emails driving traffic back) still hit the cache.
//
// Added in Step 5 (like module).
package cache

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// LikeCache wraps the Redis client with like-module-specific helpers.
// stateTTL is injected from config (was hardcoded 7d constant before Step 13).
type LikeCache struct {
	rdb      *redis.Client
	stateTTL time.Duration
}

// NewLikeCache constructs a LikeCache backed by the given Redis client.
// stateTTL is the sliding TTL for like:set:* and like:cnt:* keys (see file header).
func NewLikeCache(rdb *redis.Client, stateTTL time.Duration) *LikeCache {
	return &LikeCache{rdb: rdb, stateTTL: stateTTL}
}

// ── Key builders ────────────────────────────────────────────────────────────

// keyLikeSet returns the SET key holding all user_ids who currently like this post.
func keyLikeSet(postID int64) string {
	return "like:set:" + strconv.FormatInt(postID, 10)
}

// keyLikeCount returns the String key holding the current like count for this post.
func keyLikeCount(postID int64) string {
	return "like:cnt:" + strconv.FormatInt(postID, 10)
}

// ── Idempotency check ──────────────────────────────────────────────────────

// HasLiked reports whether userID has already liked postID.
//
// Returns (false, nil) on a Redis miss (key doesn't exist) — that's the
// correct semantics: "no entry for this post means nobody has liked it
// in the cache". The service layer treats this as "proceed with SADD";
// if a stale MySQL row exists, INSERT IGNORE in LikeConsumer absorbs it.
//
// Returns a wrapped error on Redis failure. The service layer is expected
// to fail-close on this (return 500) rather than guess — a wrong answer
// here causes either spurious double-counts or "ghost" un-likes.
func (c *LikeCache) HasLiked(ctx context.Context, postID, userID int64) (bool, error) {
	exists, err := c.rdb.SIsMember(ctx, keyLikeSet(postID), userID).Result()
	if err != nil {
		return false, fmt.Errorf("redis SISMEMBER like:set: %w", err)
	}
	return exists, nil
}

// ── Mutation: SADD + INCR (Like) ───────────────────────────────────────────

// addLikeScript atomically SADDs the user to the like set and INCRs the
// counter ONLY if the SADD actually inserted a new member (returned 1).
//
// This fixes the SADD+INCR non-atomicity in the previous pipeline-based
// implementation (Step 13). The old code always INCRd even when SADD
// returned 0 (duplicate), causing an off-by-one on the counter.
//
// Symmetry with RemoveLike's Lua script: both mutations are now atomic.
//
// KEYS[1] = like:set:{post_id}
// KEYS[2] = like:cnt:{post_id}
// ARGV[1] = userID as string
// ARGV[2] = TTL in seconds
//
// Returns the current count (after INCR if new member, unchanged otherwise).
const addLikeScript = `
local added = redis.call('SADD', KEYS[1], ARGV[1])
redis.call('EXPIRE', KEYS[1], tonumber(ARGV[2]))
if added == 0 then
    local v = redis.call('GET', KEYS[2])
    if not v then return 0 end
    return tonumber(v)
end
local n = redis.call('INCR', KEYS[2])
redis.call('EXPIRE', KEYS[2], tonumber(ARGV[2]))
return n
`

// AddLike adds userID to the post's like set and increments the counter.
//
// Uses a Lua script to ensure SADD and conditional INCR are atomic:
// INCR is skipped when SADD returns 0 (member already present), preventing
// the off-by-one that the previous pipeline implementation produced when
// concurrent or duplicate like requests slipped past the SISMEMBER guard.
//
// Returns the new count for the caller's response payload.
func (c *LikeCache) AddLike(ctx context.Context, postID, userID int64) (uint32, error) {
	res, err := c.rdb.Eval(ctx, addLikeScript,
		[]string{keyLikeSet(postID), keyLikeCount(postID)},
		strconv.FormatInt(userID, 10),
		int64(c.stateTTL.Seconds()),
	).Result()
	if err != nil {
		return 0, fmt.Errorf("redis EVAL AddLike: %w", err)
	}
	n, ok := res.(int64)
	if !ok || n < 0 {
		return 0, nil
	}
	return uint32(n), nil
}

// ── Mutation: SREM + DECR (Unlike) ─────────────────────────────────────────

// RemoveLike removes userID from the post's like set and decrements the counter.
// Both ops are issued in a pipeline to halve the round-trips.
//
// We DON'T let the counter go below 0. A naked DECR on a missing key
// would yield -1, which is then visible to the front-end. We use a Lua
// script (a tiny one — three lines) to clamp at 0. This is the only
// place LikeCache uses Lua; the asymmetry with AddLike is intentional:
//   - AddLike's race produces an over-count that LikeConsumer corrects later.
//   - RemoveLike's race could produce a *visible negative*, which is a UX
//     bug front-ends won't gracefully handle. Worth the script.
//
// Returns the new count.
func (c *LikeCache) RemoveLike(ctx context.Context, postID, userID int64) (uint32, error) {
	// SREM in pipeline; DECR-clamp-at-zero in Lua.
	pipe := c.rdb.Pipeline()
	pipe.SRem(ctx, keyLikeSet(postID), userID)
	pipe.Expire(ctx, keyLikeSet(postID), c.stateTTL)
	if _, err := pipe.Exec(ctx); err != nil {
		return 0, fmt.Errorf("redis pipeline RemoveLike (SREM): %w", err)
	}

	// Lua: if the key doesn't exist or is 0, stay at 0; otherwise DECR.
	// Returns the new value.
	const decrClampScript = `
local v = redis.call('GET', KEYS[1])
if (not v) or (tonumber(v) <= 0) then
    redis.call('SET', KEYS[1], 0, 'EX', tonumber(ARGV[1]))
    return 0
end
local n = redis.call('DECR', KEYS[1])
redis.call('EXPIRE', KEYS[1], tonumber(ARGV[1]))
return n
`
	res, err := c.rdb.Eval(ctx, decrClampScript, []string{keyLikeCount(postID)}, int64(c.stateTTL.Seconds())).Result()
	if err != nil {
		return 0, fmt.Errorf("redis EVAL DECR-clamp: %w", err)
	}
	n, ok := res.(int64)
	if !ok || n < 0 {
		return 0, nil
	}
	return uint32(n), nil
}

// ── Read: GetLikeCount ─────────────────────────────────────────────────────

// GetLikeCount returns the post's current like count.
//
// Returns (0, nil) on a cache miss — that's the correct UI behaviour:
// a post with no Redis counter has no Redis-tracked likes, so 0 is shown
// while LikeService backfills from the MySQL ground truth (not implemented
// in MVP because the only Read path is GET /like which doesn't currently
// surface count without checking SISMEMBER first; if it ever does, the
// fallback hook plugs in here).
func (c *LikeCache) GetLikeCount(ctx context.Context, postID int64) (uint32, error) {
	v, err := c.rdb.Get(ctx, keyLikeCount(postID)).Int64()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return 0, nil
		}
		return 0, fmt.Errorf("redis GET like:cnt: %w", err)
	}
	if v < 0 {
		return 0, nil
	}
	return uint32(v), nil
}
