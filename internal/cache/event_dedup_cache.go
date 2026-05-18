// Package cache — event_dedup_cache.go is the consumer-side duplicate-delivery
// guard. RabbitMQ is at-least-once, so any consumer that does a non-idempotent
// write (UPDATE … + delta, INSERT-then-update) MUST dedup on the event ID.
//
// Like-side idempotency is already guarded by Lua scripts on the cache key
// (user×post is unique), so LikeConsumer does not need this. PV / Count are
// the consumers that issue `+ ?` increments unconditionally — their flush
// would double-count under at-least-once redelivery without this guard.
//
// Key format: dedup:<topic>:<event_id>     TTL: cfg.Cache.DedupTTL (24h).
// 24h covers the RabbitMQ requeue / DLX retry window with margin; if an
// event is delivered for a third time more than 24h after the first, the
// counter drift is one increment — acceptable given the periodic reconcile
// job will realign.
//
// Added in Step 18 (debt cleanup A6 — IDEMP-01).
package cache

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// EventDedupCache is a tiny Redis-backed "have I seen this event id?" lookup.
//
// Only one operation matters: atomic check-and-mark. We deliberately do NOT
// expose a separate `IsSeen` followed by `Mark` — that two-step would race
// between concurrent deliveries of the same event id on different goroutines
// (RabbitMQ's prefetch + multiple consumers).
type EventDedupCache struct {
	rdb *redis.Client
	ttl time.Duration
}

// NewEventDedupCache constructs the cache. ttl is cfg.Cache.DedupTTL.
func NewEventDedupCache(rdb *redis.Client, ttl time.Duration) *EventDedupCache {
	return &EventDedupCache{rdb: rdb, ttl: ttl}
}

// keyEventDedup builds the dedup key. Topic is included so two events from
// different topics that happen to share an event id (vanishingly unlikely
// with UUIDv4, but defence in depth) do not collide.
func keyEventDedup(topic, eventID string) string {
	return "dedup:" + topic + ":" + eventID
}

// SeenOrMark atomically reports whether the given event id was already
// processed, marking it as seen for the configured TTL when it wasn't.
//
//	alreadySeen=true  → caller MUST drop the event (it has been handled).
//	alreadySeen=false → caller proceeds normally; this call already wrote
//	                    the dedup marker, so any concurrent redelivery will
//	                    observe alreadySeen=true.
//
// Empty eventID means the publisher omitted it — we let the event through
// rather than block all such events. This is a best-effort guard; the
// publishers in this codebase always set EventID.
//
// Redis failure: returns (false, err). Consumers treat this as "fail open"
// (process the event) — the alternative (drop) would silently lose data
// on every Redis hiccup, which is worse than rare double-count.
func (c *EventDedupCache) SeenOrMark(ctx context.Context, topic, eventID string) (bool, error) {
	if eventID == "" {
		return false, nil
	}
	ok, err := c.rdb.SetNX(ctx, keyEventDedup(topic, eventID), 1, c.ttl).Result()
	if err != nil {
		return false, fmt.Errorf("redis SETNX %s: %w", keyEventDedup(topic, eventID), err)
	}
	// SETNX semantics:
	//   ok=true  → key was absent and is now set → first delivery.
	//   ok=false → key already present           → duplicate delivery.
	return !ok, nil
}
