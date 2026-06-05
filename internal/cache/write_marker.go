// Package cache — write_marker.go closes the read-after-write race that DBResolver
// read-write splitting introduces. ([Fix #1])
//
// Problem:
//   The service writes through the master, but reads default to the slaves (Random
//   policy). Replication lag is usually <10ms but can spike under load. When a
//   user creates a post then immediately fetches their author feed, the read can
//   land on a slave that hasn't applied the binlog yet → the post seems to have
//   vanished.
//
// Fix:
//   Each write stamps a short-lived "write marker" key in Redis keyed by the
//   actor's userID. Reads that care about freshness probe the marker; if it's
//   present, the read forces routing to the master (via repository.WithForceMaster
//   on the context). The TTL (5s) is comfortably above worst-case lag observed in
//   prod and short enough that 100% master routing only happens for that user
//   immediately after their write.
//
// This is intentionally cheap: a single SET (write side) and a single EXISTS
// (read side), both single-digit-ms. We accept the rare false positive (the user
// shifts to master reads briefly even when there is no real lag) — the
// alternative (stale-read incident) is dramatically worse for UX.
//
// Scope of the marker:
//   - Per-user, not per-object. If user A creates a post, only A's subsequent
//     reads route to master; user B's feed is unaffected.
//   - 5 seconds. Long enough to absorb worst-case replication catch-up (we
//     observe Seconds_Behind_Source <2s under normal load), short enough that
//     load spikes don't drag long onto master.
package cache

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// WriteMarker tracks recent writes per user so subsequent reads can opt into
// master routing for a brief window. See file header for the rationale.
type WriteMarker struct {
	rdb *redis.Client
	ttl time.Duration
}

// NewWriteMarker constructs a WriteMarker. ttl is the freshness window
// (recommended 5s; longer drags load onto master, shorter risks stale reads
// under replication-lag spikes).
func NewWriteMarker(rdb *redis.Client, ttl time.Duration) *WriteMarker {
	return &WriteMarker{rdb: rdb, ttl: ttl}
}

func keyWriteMarker(userID int64) string {
	return fmt.Sprintf("write:user:%d", userID)
}

// Mark records that userID just performed a write. Best-effort: a Redis hiccup
// degrades to "the next read may hit a slave with stale data", which is the
// status quo without this layer — never worse.
func (m *WriteMarker) Mark(ctx context.Context, userID int64) {
	if err := m.rdb.Set(ctx, keyWriteMarker(userID), "1", m.ttl).Err(); err != nil {
		// Caller does not propagate — the underlying write already succeeded.
		// Logging is the caller's choice (they have request-scoped context).
		_ = err
	}
}

// Has reports whether userID has a fresh write within the marker window.
// On Redis failure returns false (open-loop: fall back to slave routing — same
// behaviour as before this layer existed).
func (m *WriteMarker) Has(ctx context.Context, userID int64) bool {
	n, err := m.rdb.Exists(ctx, keyWriteMarker(userID)).Result()
	if err != nil {
		if !errors.Is(err, redis.Nil) {
			return false
		}
	}
	return n > 0
}
