// Package consumer — count_consumer.go consumes TopicPost + TopicLike +
// TopicUnlike events and maintains the redundant counters on the users table:
//
//	users.post_count    — incremented on every TopicPost event
//	users.total_likes   — incremented on TopicLike, decremented on TopicUnlike
//
// ── Why combine three topics into one consumer ─────────────────────────────
//
// All three topics produce the same kind of mutation: an UPDATE to a single
// users row by a single column +1 / -1 delta. Co-locating them lets us:
//
//   - Aggregate cross-topic deltas for the same user into ONE UPDATE.
//     A user posts (post_count+1) and gets liked (total_likes+1) within
//     the same flush window → one `UPDATE users SET post_count=post_count+1,
//     total_likes=total_likes+1 WHERE id=?` instead of two round-trips.
//
//   - Keep the connection-pool pressure from "user counter sync" bounded
//     to a single goroutine's outflow. With three separate consumers the
//     same hot user could see three concurrent UPDATEs and contend on the
//     row-level lock.
//
// ── Why TopicPost goes here, not back to PostService ────────────────────────
//
// PostService publishes PostEvent on Create() but does NOT update
// users.post_count synchronously — that would force every Create to
// touch two tables in one transaction (cross-table writes are exactly
// what the EventBus exists to avoid). CountConsumer is the home for all
// "redundant counter" maintenance, regardless of which event triggers it.
//
// ── Why GREATEST(0, ...) on total_likes decrement ─────────────────────────
//
// users.total_likes is INT UNSIGNED. Subtraction can underflow if a
// duplicate UnlikeEvent slips through (Channel mode shouldn't, but
// RabbitMQ at-least-once will). GREATEST(0, ...) clamps to zero —
// see like_repository.go's identical defense for the rationale.
//
// ── Batch sizing ───────────────────────────────────────────────────────────
//
// PRD-Phase3 §3.4: 20 events / 10s. Smaller than LikeConsumer because
// CountConsumer's writes hit the users table, which is also the auth
// hot path; we'd rather not stall login throughput with long batched
// UPDATEs. 20 events fan out to at most 20 distinct users per flush,
// well within MySQL's tolerance for a brief burst.
//
// ── Graceful shutdown ──────────────────────────────────────────────────────
//
// Three subscribe goroutines (one per topic) share a single internal
// channel of countDelta values. flushLoop drains on ctx cancel — the
// pattern is identical to LikeConsumer / PVConsumer.
//
// Added in Step 5 (like module).
package consumer

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"cooking-platform/internal/event"

	"go.uber.org/zap"
	"gorm.io/gorm"
)

const (
	countBatchSize     = 20
	countFlushInterval = 10 * time.Second
	countChannelBuf    = countBatchSize * 4
)

// countDelta is the in-memory shape used to ferry events from the
// subscribe goroutines to the batch loop. We only need the user_id
// being mutated and the columnar delta — payload-specific fields are
// already decoded by the subscribe goroutine.
type countDelta struct {
	userID     int64
	postCount  int64 // 0 or +1
	totalLikes int64 // -1, 0, or +1
}

// CountConsumer subscribes to three topics and maintains users counters.
type CountConsumer struct {
	bus event.EventSubscriber
	db  *gorm.DB
}

// NewCountConsumer constructs a CountConsumer.
func NewCountConsumer(bus event.EventSubscriber, db *gorm.DB) *CountConsumer {
	return &CountConsumer{bus: bus, db: db}
}

// Name satisfies EventConsumer.
func (c *CountConsumer) Name() string {
	return "count-consumer"
}

// Start blocks until ctx is cancelled.
func (c *CountConsumer) Start(ctx context.Context) error {
	eventCh := make(chan countDelta, countChannelBuf)

	var subWg sync.WaitGroup
	subWg.Add(3)

	// ── TopicPost → users.post_count +1 ─────────────────────────────────────
	go func() {
		defer subWg.Done()
		_ = c.bus.Subscribe(ctx, event.TopicPost, func(_ context.Context, payload []byte) error {
			var p event.PostEvent
			if err := json.Unmarshal(payload, &p); err != nil {
				zap.L().Warn("count consumer: unmarshal PostEvent", zap.Error(err))
				return nil
			}
			select {
			case eventCh <- countDelta{userID: p.AuthorID, postCount: 1}:
			case <-ctx.Done():
				return nil
			}
			return nil
		})
	}()

	// ── TopicLike → users.total_likes +1 (for AuthorID, the post owner) ────
	go func() {
		defer subWg.Done()
		_ = c.bus.Subscribe(ctx, event.TopicLike, func(_ context.Context, payload []byte) error {
			var p event.LikeEvent
			if err := json.Unmarshal(payload, &p); err != nil {
				zap.L().Warn("count consumer: unmarshal LikeEvent", zap.Error(err))
				return nil
			}
			select {
			case eventCh <- countDelta{userID: p.AuthorID, totalLikes: 1}:
			case <-ctx.Done():
				return nil
			}
			return nil
		})
	}()

	// ── TopicUnlike → users.total_likes -1 ─────────────────────────────────
	go func() {
		defer subWg.Done()
		_ = c.bus.Subscribe(ctx, event.TopicUnlike, func(_ context.Context, payload []byte) error {
			var p event.UnlikeEvent
			if err := json.Unmarshal(payload, &p); err != nil {
				zap.L().Warn("count consumer: unmarshal UnlikeEvent", zap.Error(err))
				return nil
			}
			select {
			case eventCh <- countDelta{userID: p.AuthorID, totalLikes: -1}:
			case <-ctx.Done():
				return nil
			}
			return nil
		})
	}()

	c.flushLoop(ctx, eventCh, &subWg)
	return nil
}

// flushLoop aggregates deltas by user_id and flushes on cap or tick.
func (c *CountConsumer) flushLoop(ctx context.Context, eventCh chan countDelta, subWg *sync.WaitGroup) {
	// Two parallel maps so we can compose the UPDATE per user with both
	// columns set in one statement when both have non-zero deltas.
	postDeltas := make(map[int64]int64, countBatchSize)
	likeDeltas := make(map[int64]int64, countBatchSize)
	bufCount := 0

	ticker := time.NewTicker(countFlushInterval)
	defer ticker.Stop()

	totalProcessed := 0

	flush := func(useCtx context.Context) {
		if bufCount == 0 {
			return
		}
		totalProcessed += c.flush(useCtx, postDeltas, likeDeltas)
		postDeltas = make(map[int64]int64, countBatchSize)
		likeDeltas = make(map[int64]int64, countBatchSize)
		bufCount = 0
	}

	apply := func(d countDelta) {
		if d.postCount != 0 {
			postDeltas[d.userID] += d.postCount
		}
		if d.totalLikes != 0 {
			likeDeltas[d.userID] += d.totalLikes
		}
		bufCount++
	}

	for {
		select {
		case d := <-eventCh:
			apply(d)
			if bufCount >= countBatchSize {
				flush(ctx)
			}

		case <-ticker.C:
			flush(ctx)

		case <-ctx.Done():
			subWg.Wait()
		drainLoop:
			for {
				select {
				case d := <-eventCh:
					apply(d)
				default:
					break drainLoop
				}
			}
			flush(context.Background())
			zap.L().Info("count consumer drained",
				zap.Int("total_processed", totalProcessed),
			)
			return
		}
	}
}

// flush issues one combined UPDATE per affected user.
//
// SQL shape — the most general case (both columns change):
//
//	UPDATE users
//	   SET post_count  = post_count  + ?,
//	       total_likes = GREATEST(0, CAST(total_likes AS SIGNED) + ?)
//	 WHERE id = ?
//
// When only one column has a non-zero delta we elide the other clause
// to keep the SQL minimal — micro-optimisation, but reading "UPDATE
// users SET post_count = ..." in slow logs is more grep-friendly than
// a SET-everything boilerplate.
func (c *CountConsumer) flush(ctx context.Context, postDeltas, likeDeltas map[int64]int64) int {
	// Union of user_ids touched.
	allUsers := make(map[int64]struct{}, len(postDeltas)+len(likeDeltas))
	for uid := range postDeltas {
		allUsers[uid] = struct{}{}
	}
	for uid := range likeDeltas {
		allUsers[uid] = struct{}{}
	}

	total := 0
	for uid := range allUsers {
		pd := postDeltas[uid]
		ld := likeDeltas[uid]

		if pd == 0 && ld == 0 {
			continue
		}

		var sql string
		var args []any

		switch {
		case pd != 0 && ld != 0:
			sql = "UPDATE users SET post_count = post_count + ?, " +
				"total_likes = GREATEST(0, CAST(total_likes AS SIGNED) + ?) WHERE id = ?"
			args = []any{pd, ld, uid}
		case pd != 0:
			sql = "UPDATE users SET post_count = post_count + ? WHERE id = ?"
			args = []any{pd, uid}
		case ld != 0:
			sql = "UPDATE users SET total_likes = GREATEST(0, CAST(total_likes AS SIGNED) + ?) WHERE id = ?"
			args = []any{ld, uid}
		}

		if err := c.db.WithContext(ctx).Exec(sql, args...).Error; err != nil {
			zap.L().Warn("count consumer: UPDATE users failed",
				zap.Int64("user_id", uid),
				zap.Int64("post_delta", pd),
				zap.Int64("like_delta", ld),
				zap.Error(err),
			)
			continue
		}
		// Count "events handled" — sum of absolute deltas approximates the
		// real event count, modulo aggregation. Useful for shutdown log.
		total += int(absInt64(pd) + absInt64(ld))
	}
	return total
}

// absInt64 returns |v|. Math.Abs operates on float64 so this avoids the
// round-trip; inlined easily by the compiler.
func absInt64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
