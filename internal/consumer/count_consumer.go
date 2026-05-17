// Package consumer — count_consumer.go consumes TopicPost + TopicLike +
// TopicUnlike + TopicFollow + TopicUnfollow events and maintains the
// redundant counters on the users table:
//
//	users.post_count       — incremented on every TopicPost event
//	users.total_likes      — incremented on TopicLike, decremented on TopicUnlike
//	users.following_count  — incremented/decremented on TopicFollow/TopicUnfollow
//	                         (for the FollowerID — "how many I follow")
//	users.follower_count   — incremented/decremented on TopicFollow/TopicUnfollow
//	                         (for the FollowingID — "how many follow me")
//
// ── Why combine five topics into one consumer ──────────────────────────────
//
// All five topics produce the same kind of mutation: an UPDATE to a single
// users row by a single column +1 / -1 delta. Co-locating them lets us:
//
//   - Aggregate cross-topic deltas for the same user into ONE UPDATE.
//     A user posts (post_count+1) and gets a new follower (follower_count+1)
//     within the same flush window → one combined UPDATE instead of two
//     round-trips.
//
//   - Keep the connection-pool pressure from "user counter sync" bounded
//     to a single goroutine's outflow. With separate consumers the same hot
//     user could see concurrent UPDATEs and contend on the row-level lock.
//
// ── Why TopicPost / TopicFollow go here, not back to their services ────────
//
// PostService publishes PostEvent on Create() and FollowService publishes
// Follow/UnfollowEvent — but neither updates the users counters
// synchronously. That would force every Create / Follow to touch two tables
// in one transaction (cross-table writes are exactly what the EventBus
// exists to avoid). CountConsumer is the single home for all "redundant
// counter" maintenance, regardless of which event triggers it.
//
// ── Why one FollowEvent produces TWO countDeltas ───────────────────────────
//
// A follow edge changes two users' counters at once: the follower's
// following_count and the followee's follower_count. The TopicFollow
// subscribe goroutine therefore emits two countDelta values per event (and
// TopicUnfollow likewise, with -1). They flow through the same channel and
// aggregation as every other delta — no special-casing in flushLoop.
//
// ── Why GREATEST(0, ...) on every decrementable column ─────────────────────
//
// total_likes / follower_count / following_count are INT UNSIGNED.
// Subtraction can underflow if a duplicate Unlike/Unfollow event slips
// through (Channel mode shouldn't, but RabbitMQ at-least-once will).
// GREATEST(0, CAST(... AS SIGNED) + ?) clamps to zero — see
// like_repository.go's identical defense for the rationale. post_count is
// append-only (no event ever decrements it) so it needs no clamp.
//
// ── Batch sizing ───────────────────────────────────────────────────────────
//
// PRD-Phase3 §3.4: 20 events / 10s. Smaller than LikeConsumer because
// CountConsumer's writes hit the users table, which is also the auth hot
// path; we'd rather not stall login throughput with long batched UPDATEs.
// Note one follow/unfollow event counts as two countDeltas toward the
// batch, so a burst of follows fills the batch twice as fast — intended,
// it keeps the per-flush user fan-out bounded.
//
// ── Graceful shutdown ──────────────────────────────────────────────────────
//
// Five subscribe goroutines (one per topic) share a single internal channel
// of countDelta values. flushLoop drains on ctx cancel — the pattern is
// identical to LikeConsumer / PVConsumer.
//
// Added in Step 5 (like module). Extended in Step 8 (follow module) to also
// consume TopicFollow / TopicUnfollow.
package consumer

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"cooking-platform/internal/event"
	"cooking-platform/pkg/config"
	"cooking-platform/pkg/metrics"

	"go.uber.org/zap"
	"gorm.io/gorm"
)

// countDelta is the in-memory shape used to ferry events from the subscribe
// goroutines to the batch loop. We only need the user_id being mutated and
// the per-column delta — payload-specific fields are already decoded by the
// subscribe goroutine.
//
// Each field is the signed delta for one users column. A single event sets
// exactly one field (a follow/unfollow event produces TWO countDelta values,
// one per affected user, each setting one field).
type countDelta struct {
	userID         int64
	postCount      int64 // 0 or +1
	totalLikes     int64 // -1, 0, or +1
	followerCount  int64 // -1, 0, or +1
	followingCount int64 // -1, 0, or +1
}

// CountConsumer subscribes to five topics and maintains users counters.
type CountConsumer struct {
	bus           event.EventSubscriber
	db            *gorm.DB
	batchSize     int
	flushInterval time.Duration
}

// NewCountConsumer constructs a CountConsumer.
// cfg provides batch/flush knobs previously hardcoded as package-level constants.
func NewCountConsumer(bus event.EventSubscriber, db *gorm.DB, cfg config.CountConsumerConfig) *CountConsumer {
	return &CountConsumer{
		bus:           bus,
		db:            db,
		batchSize:     cfg.BatchSize,
		flushInterval: cfg.FlushInterval,
	}
}

// Name satisfies EventConsumer.
func (c *CountConsumer) Name() string {
	return "count-consumer"
}

// Start blocks until ctx is cancelled.
func (c *CountConsumer) Start(ctx context.Context) error {
	eventCh := make(chan countDelta, c.batchSize*4)

	var subWg sync.WaitGroup
	subWg.Add(5)

	// ── TopicPost → users.post_count +1 ─────────────────────────────────────
	go func() {
		defer subWg.Done()
		_ = c.bus.Subscribe(ctx, event.TopicPost, func(_ context.Context, payload []byte) error {
			var p event.PostEvent
			if err := json.Unmarshal(payload, &p); err != nil {
				zap.L().Warn("count consumer: unmarshal PostEvent", zap.Error(err))
				return nil
			}
			c.send(ctx, eventCh, countDelta{userID: p.AuthorID, postCount: 1}, event.TopicPost)
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
			c.send(ctx, eventCh, countDelta{userID: p.AuthorID, totalLikes: 1}, event.TopicLike)
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
			c.send(ctx, eventCh, countDelta{userID: p.AuthorID, totalLikes: -1}, event.TopicUnlike)
			return nil
		})
	}()

	// ── TopicFollow → following_count +1 (follower) & follower_count +1 ────
	// One follow edge mutates two users, so this handler emits two deltas.
	go func() {
		defer subWg.Done()
		_ = c.bus.Subscribe(ctx, event.TopicFollow, func(_ context.Context, payload []byte) error {
			var p event.FollowEvent
			if err := json.Unmarshal(payload, &p); err != nil {
				zap.L().Warn("count consumer: unmarshal FollowEvent", zap.Error(err))
				return nil
			}
			c.send(ctx, eventCh, countDelta{userID: p.FollowerID, followingCount: 1}, event.TopicFollow)
			c.send(ctx, eventCh, countDelta{userID: p.FollowingID, followerCount: 1}, event.TopicFollow)
			return nil
		})
	}()

	// ── TopicUnfollow → following_count -1 (follower) & follower_count -1 ──
	go func() {
		defer subWg.Done()
		_ = c.bus.Subscribe(ctx, event.TopicUnfollow, func(_ context.Context, payload []byte) error {
			var p event.UnfollowEvent
			if err := json.Unmarshal(payload, &p); err != nil {
				zap.L().Warn("count consumer: unmarshal UnfollowEvent", zap.Error(err))
				return nil
			}
			c.send(ctx, eventCh, countDelta{userID: p.FollowerID, followingCount: -1}, event.TopicUnfollow)
			c.send(ctx, eventCh, countDelta{userID: p.FollowingID, followerCount: -1}, event.TopicUnfollow)
			return nil
		})
	}()

	c.flushLoop(ctx, eventCh, &subWg)
	return nil
}

// send pushes one delta onto eventCh, honouring ctx cancellation so a
// subscribe goroutine never blocks forever on a full channel during
// shutdown. Extracted in Step 8 because the follow handlers each emit two
// deltas — duplicating the select block four times was noise.
// topic is used only for metrics labelling; it may be empty string to skip.
func (c *CountConsumer) send(ctx context.Context, eventCh chan countDelta, d countDelta, topic string) {
	select {
	case eventCh <- d:
		if metrics.ConsumerProcessedTotal != nil && topic != "" {
			metrics.ConsumerProcessedTotal.WithLabelValues(c.Name(), topic).Inc()
		}
	case <-ctx.Done():
	}
}

// flushLoop aggregates deltas by user_id and flushes on cap or tick.
func (c *CountConsumer) flushLoop(ctx context.Context, eventCh chan countDelta, subWg *sync.WaitGroup) {
	// One map per counter column so the per-user UPDATE can set every
	// changed column in a single statement.
	postDeltas := make(map[int64]int64, c.batchSize)
	likeDeltas := make(map[int64]int64, c.batchSize)
	followerDeltas := make(map[int64]int64, c.batchSize)
	followingDeltas := make(map[int64]int64, c.batchSize)
	bufCount := 0

	ticker := time.NewTicker(c.flushInterval)
	defer ticker.Stop()

	totalProcessed := 0

	flush := func(useCtx context.Context) {
		if bufCount == 0 {
			return
		}
		totalProcessed += c.flush(useCtx, postDeltas, likeDeltas, followerDeltas, followingDeltas)
		postDeltas = make(map[int64]int64, c.batchSize)
		likeDeltas = make(map[int64]int64, c.batchSize)
		followerDeltas = make(map[int64]int64, c.batchSize)
		followingDeltas = make(map[int64]int64, c.batchSize)
		bufCount = 0
	}

	apply := func(d countDelta) {
		if d.postCount != 0 {
			postDeltas[d.userID] += d.postCount
		}
		if d.totalLikes != 0 {
			likeDeltas[d.userID] += d.totalLikes
		}
		if d.followerCount != 0 {
			followerDeltas[d.userID] += d.followerCount
		}
		if d.followingCount != 0 {
			followingDeltas[d.userID] += d.followingCount
		}
		bufCount++
	}

	for {
		select {
		case d := <-eventCh:
			apply(d)
			if bufCount >= c.batchSize {
				flush(ctx)
			}

		case <-ticker.C:
			flush(ctx)
			if metrics.ConsumerQueueDepth != nil {
				metrics.ConsumerQueueDepth.WithLabelValues(c.Name()).Set(float64(len(eventCh)))
			}

		case <-ctx.Done():
			subWg.Wait()
			DrainChan(eventCh, apply)
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
// SQL shape — the most general case (all four columns change):
//
//	UPDATE users
//	   SET post_count      = post_count + ?,
//	       total_likes     = GREATEST(0, CAST(total_likes AS SIGNED) + ?),
//	       follower_count  = GREATEST(0, CAST(follower_count AS SIGNED) + ?),
//	       following_count = GREATEST(0, CAST(following_count AS SIGNED) + ?)
//	 WHERE id = ?
//
// The SET clause is assembled dynamically: only columns with a non-zero
// delta for that user are included. Step 5 used a hand-written switch over
// the (post, like) combinations; Step 8's four columns would balloon that
// to 15 cases, so the switch was replaced with a clause-builder. The
// substance is unchanged — still one aggregated UPDATE per user, still
// GREATEST-clamped on every unsigned-decrementable column. Building "UPDATE
// users SET <only changed columns>" also keeps slow-log lines grep-friendly.
//
// post_count is the one column with no GREATEST guard: no event ever emits a
// negative post delta, so it cannot underflow.
func (c *CountConsumer) flush(ctx context.Context, postDeltas, likeDeltas, followerDeltas, followingDeltas map[int64]int64) int {
	// Union of all user_ids touched across the four maps.
	allUsers := make(map[int64]struct{}, len(postDeltas)+len(likeDeltas)+len(followerDeltas)+len(followingDeltas))
	for uid := range postDeltas {
		allUsers[uid] = struct{}{}
	}
	for uid := range likeDeltas {
		allUsers[uid] = struct{}{}
	}
	for uid := range followerDeltas {
		allUsers[uid] = struct{}{}
	}
	for uid := range followingDeltas {
		allUsers[uid] = struct{}{}
	}

	total := 0
	for uid := range allUsers {
		pd := postDeltas[uid]
		ld := likeDeltas[uid]
		fd := followerDeltas[uid]
		gd := followingDeltas[uid]

		// setClauses / args are built in lockstep — each appended clause has
		// exactly one matching placeholder arg, in the same order.
		setClauses := make([]string, 0, 4)
		args := make([]any, 0, 5)

		if pd != 0 {
			// Append-only counter — no clamp needed.
			setClauses = append(setClauses, "post_count = post_count + ?")
			args = append(args, pd)
		}
		if ld != 0 {
			setClauses = append(setClauses, "total_likes = GREATEST(0, CAST(total_likes AS SIGNED) + ?)")
			args = append(args, ld)
		}
		if fd != 0 {
			setClauses = append(setClauses, "follower_count = GREATEST(0, CAST(follower_count AS SIGNED) + ?)")
			args = append(args, fd)
		}
		if gd != 0 {
			setClauses = append(setClauses, "following_count = GREATEST(0, CAST(following_count AS SIGNED) + ?)")
			args = append(args, gd)
		}

		if len(setClauses) == 0 {
			continue
		}

		sql := "UPDATE users SET " + strings.Join(setClauses, ", ") + " WHERE id = ?"
		args = append(args, uid)

		if err := c.db.WithContext(ctx).Exec(sql, args...).Error; err != nil {
			zap.L().Warn("count consumer: UPDATE users failed",
				zap.Int64("user_id", uid),
				zap.Int64("post_delta", pd),
				zap.Int64("like_delta", ld),
				zap.Int64("follower_delta", fd),
				zap.Int64("following_delta", gd),
				zap.Error(err),
			)
			continue
		}
		// Count "events handled" — sum of absolute deltas approximates the
		// real event count, modulo aggregation. Useful for the shutdown log.
		total += int(absInt64(pd) + absInt64(ld) + absInt64(fd) + absInt64(gd))
	}
	return total
}

// absInt64 returns |v|. math.Abs operates on float64 so this avoids the
// round-trip; inlined easily by the compiler.
func absInt64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
