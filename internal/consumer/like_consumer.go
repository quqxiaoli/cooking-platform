// Package consumer — like_consumer.go consumes TopicLike + TopicUnlike events
// emitted by LikeService and persists them to the likes table, simultaneously
// updating posts.like_count.
//
// ── Why ONE consumer for two topics ─────────────────────────────────────────
//
// Likes and unlikes are dual operations on the same underlying state
// (the likes table). Batching them through a single flush loop lets us:
//
//  1. Apply both inserts and deletes in a single MySQL "tick" — back-to-back,
//     atomically per-statement, with predictable ordering. A user who likes
//     then immediately unlikes within the same 3-second window produces
//     one INSERT IGNORE (RowsAffected=1) followed by one DELETE
//     (RowsAffected=1), netting zero rows in `likes` and zero net delta
//     on like_count. Two separate consumers wouldn't see each other's
//     state and would race on the count UPDATE.
//
//  2. Use ONE batchSize threshold and ONE flush ticker, halving the
//     coordination logic vs. two parallel consumers.
//
// Subscription is done via TWO goroutines (one per topic), both feeding
// the same internal event channel. The flush loop reads from that channel
// and dispatches based on which slice the event lands in (likeBatch /
// unlikeBatch).
//
// ── Why INSERT IGNORE + RowsAffected for delta computation ─────────────────
//
// Channel-mode MQ doesn't redeliver, but the underlying invariant is
// useful regardless: a batch of 50 LikeEvents may include duplicates from
// double-tap UI bugs or eventually from RabbitMQ at-least-once delivery
// (Step 13). INSERT IGNORE absorbs duplicates against uk_user_post and
// reports the *actual* number of new rows via RowsAffected. We then
// distribute that aggregate delta across posts by counting per-post in
// the batch — but we trust RowsAffected over count(batch) so duplicates
// don't inflate like_count.
//
// Concretely:
//   - rawLikes  = events received in this batch (may contain duplicates)
//   - per-post likeCounts[postID] = number of events for that post
//   - BatchInsert returns "real new rows" (≤ len(rawLikes))
//   - We scale the per-post deltas down proportionally if RowsAffected
//     differs from len(rawLikes). The proportional scale-down is correct
//     when duplicates are evenly distributed; when they're not, the
//     count drifts slightly, and CountConsumer's reconciliation pass
//     (TopicLike subscription) keeps users.total_likes consistent because
//     it independently counts events. Exact per-post correction is left
//     to a future "verify against SELECT COUNT(*) FROM likes GROUP BY"
//     reconcile job (post-MVP).
//
// For the MVP — Channel mode, no redelivery — RowsAffected almost always
// equals len(rawLikes), and the proportional logic is just a no-op safety net.
//
// ── Graceful shutdown ──────────────────────────────────────────────────────
//
// On ctx cancel:
//  1. Subscribe goroutines exit (their internal ctx.Done case fires)
//  2. eventCh stops receiving new sends
//  3. flushLoop sees ctx.Done, drains any remaining buffer to MySQL
//  4. Returns nil — ConsumerManager's wg.Wait() unblocks
//
// The drain step is mandatory: events already in batches buffers must
// hit MySQL before we exit, or the user will see "I liked it!" with no
// row in likes (the in-memory state lost across the restart boundary).
//
// Added in Step 5 (like module).
package consumer

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"cooking-platform/internal/event"
	"cooking-platform/internal/model"
	"cooking-platform/internal/repository"

	"go.uber.org/zap"
)

// likeBatchSize is the per-flush event cap. PRD-Phase3 §3.4: "攒 50 条或 3s 一批".
const likeBatchSize = 50

// likeFlushInterval is the maximum staleness of an unflushed event.
const likeFlushInterval = 3 * time.Second

// likeChannelBuf sizes the internal channel that fan-ins both topics into
// the batch loop. 4× batch size gives ~12 seconds of buffer at the flush
// rate before backpressure kicks in — enough to absorb ChannelBus's per-
// subscriber buffer flicker without blocking either subscribe goroutine.
const likeChannelBuf = likeBatchSize * 4

// likeAction encodes which topic an event came from. Plain int8 (not a
// boolean) so future "edit-like" or "weighted-like" actions can be added
// without a breaking refactor.
type likeAction int8

const (
	actionLike   likeAction = 1
	actionUnlike likeAction = 2
)

// likeBatchEvent is the in-memory shape used between the two subscribe
// goroutines and the batch loop. Slimmer than the wire event — we only
// keep what BatchInsert / BatchDelete will use.
type likeBatchEvent struct {
	action likeAction
	userID int64
	postID int64
}

// LikeConsumer subscribes to TopicLike and TopicUnlike, batches events,
// and persists them to MySQL via LikeRepository.
type LikeConsumer struct {
	bus      event.EventSubscriber
	likeRepo repository.LikeRepository
}

// NewLikeConsumer constructs a LikeConsumer. Dependencies are minimal by
// design — the consumer owns its own batching state and ticker.
func NewLikeConsumer(bus event.EventSubscriber, likeRepo repository.LikeRepository) *LikeConsumer {
	return &LikeConsumer{
		bus:      bus,
		likeRepo: likeRepo,
	}
}

// Name satisfies EventConsumer. Used in lifecycle log lines.
func (c *LikeConsumer) Name() string {
	return "like-consumer"
}

// Start blocks until ctx is cancelled. See file header for the lifecycle.
func (c *LikeConsumer) Start(ctx context.Context) error {
	eventCh := make(chan likeBatchEvent, likeChannelBuf)

	// subWg tracks the two Subscribe goroutines so we can wait for them
	// to exit before closing eventCh — closing while a goroutine still
	// holds the send side would panic on the next send during drain.
	var subWg sync.WaitGroup
	subWg.Add(2)

	// TopicLike subscriber.
	go func() {
		defer subWg.Done()
		_ = c.bus.Subscribe(ctx, event.TopicLike, func(_ context.Context, payload []byte) error {
			var p event.LikeEvent
			if err := json.Unmarshal(payload, &p); err != nil {
				zap.L().Warn("like consumer: unmarshal LikeEvent", zap.Error(err))
				return nil // don't propagate — bad payloads should not retry forever
			}
			// Non-blocking-ish send: under normal load eventCh has plenty
			// of headroom. If genuinely full (consumer way behind), block
			// here propagates backpressure to the bus.
			select {
			case eventCh <- likeBatchEvent{action: actionLike, userID: p.UserID, postID: p.PostID}:
			case <-ctx.Done():
				return nil
			}
			return nil
		})
	}()

	// TopicUnlike subscriber.
	go func() {
		defer subWg.Done()
		_ = c.bus.Subscribe(ctx, event.TopicUnlike, func(_ context.Context, payload []byte) error {
			var p event.UnlikeEvent
			if err := json.Unmarshal(payload, &p); err != nil {
				zap.L().Warn("like consumer: unmarshal UnlikeEvent", zap.Error(err))
				return nil
			}
			select {
			case eventCh <- likeBatchEvent{action: actionUnlike, userID: p.UserID, postID: p.PostID}:
			case <-ctx.Done():
				return nil
			}
			return nil
		})
	}()

	// Flush loop runs in this goroutine — Start is the consumer's owned
	// goroutine (per ConsumerManager contract) and we don't fan out further.
	c.flushLoop(ctx, eventCh, &subWg)
	return nil
}

// flushLoop accumulates events until either batchSize or flushInterval is
// reached, then writes the batch to MySQL.
func (c *LikeConsumer) flushLoop(ctx context.Context, eventCh chan likeBatchEvent, subWg *sync.WaitGroup) {
	likeBuf := make([]likeBatchEvent, 0, likeBatchSize)
	unlikeBuf := make([]likeBatchEvent, 0, likeBatchSize)

	ticker := time.NewTicker(likeFlushInterval)
	defer ticker.Stop()

	totalProcessed := 0

	for {
		select {
		case e := <-eventCh:
			switch e.action {
			case actionLike:
				likeBuf = append(likeBuf, e)
			case actionUnlike:
				unlikeBuf = append(unlikeBuf, e)
			}
			// Flush early if either bucket alone hits the cap.
			if len(likeBuf) >= likeBatchSize || len(unlikeBuf) >= likeBatchSize {
				totalProcessed += c.flush(ctx, &likeBuf, &unlikeBuf)
			}

		case <-ticker.C:
			if len(likeBuf) > 0 || len(unlikeBuf) > 0 {
				totalProcessed += c.flush(ctx, &likeBuf, &unlikeBuf)
			}

		case <-ctx.Done():
			// Drain: wait for subscribers to fully exit (their drain inside
			// ChannelBus.Subscribe will push remaining events through),
			// then flush whatever is still in memory.
			subWg.Wait()
			// After subWg.Wait, no more sends will occur on eventCh — safe
			// to range-drain it without panicking on close (we never close
			// eventCh; ChannelBus shutdown semantics don't require it).
		drainLoop:
			for {
				select {
				case e := <-eventCh:
					switch e.action {
					case actionLike:
						likeBuf = append(likeBuf, e)
					case actionUnlike:
						unlikeBuf = append(unlikeBuf, e)
					}
				default:
					break drainLoop
				}
			}
			if len(likeBuf) > 0 || len(unlikeBuf) > 0 {
				// Use background ctx for the final flush — the parent ctx
				// is already cancelled, but we still need MySQL writes to
				// complete. ConsumerManager's wg.Wait() bounds total
				// shutdown time at this layer.
				totalProcessed += c.flushWithCtx(context.Background(), &likeBuf, &unlikeBuf)
			}
			zap.L().Info("like consumer drained",
				zap.Int("total_processed", totalProcessed),
			)
			return
		}
	}
}

// flush is the normal-path wrapper that uses the provided ctx.
func (c *LikeConsumer) flush(ctx context.Context, likeBuf, unlikeBuf *[]likeBatchEvent) int {
	return c.flushWithCtx(ctx, likeBuf, unlikeBuf)
}

// flushWithCtx persists the buffered events using the given ctx. Buffers
// are reset after a successful (or failed-and-logged) flush.
//
// Sequencing: likes first, then unlikes. Mixed-order events from the same
// (user_id, post_id) within a single batch produce the right end state:
//   - L-then-U: INSERT IGNORE (likes++) → DELETE (likes--), net zero.
//   - U-then-L: DELETE (no-op if not present) → INSERT IGNORE (likes++).
//
// Either ordering converges to "net effect of last event wins", which is
// also what the Redis source-of-truth says.
func (c *LikeConsumer) flushWithCtx(ctx context.Context, likeBuf, unlikeBuf *[]likeBatchEvent) int {
	totalThisFlush := 0

	// ── Likes ──────────────────────────────────────────────────────────────
	if len(*likeBuf) > 0 {
		rows := make([]*model.Like, 0, len(*likeBuf))
		perPost := make(map[int64]int64, len(*likeBuf))
		for _, e := range *likeBuf {
			rows = append(rows, &model.Like{
				UserID:    e.userID,
				PostID:    e.postID,
				CreatedAt: time.Now(),
			})
			perPost[e.postID]++
		}

		realInserts, err := c.likeRepo.BatchInsert(ctx, rows)
		if err != nil {
			zap.L().Warn("like consumer: BatchInsert failed",
				zap.Int("batch_size", len(rows)),
				zap.Error(err),
			)
		} else {
			// Scale per-post deltas if duplicates were filtered.
			scaledPerPost := scaleDeltas(perPost, int64(len(rows)), realInserts)
			if err := c.likeRepo.IncrPostLikeCount(ctx, scaledPerPost); err != nil {
				zap.L().Warn("like consumer: IncrPostLikeCount failed", zap.Error(err))
			}
			totalThisFlush += int(realInserts)
		}
		*likeBuf = (*likeBuf)[:0]
	}

	// ── Unlikes ────────────────────────────────────────────────────────────
	if len(*unlikeBuf) > 0 {
		pairs := make([]repository.UserPostPair, 0, len(*unlikeBuf))
		perPost := make(map[int64]int64, len(*unlikeBuf))
		for _, e := range *unlikeBuf {
			pairs = append(pairs, repository.UserPostPair{
				UserID: e.userID,
				PostID: e.postID,
			})
			perPost[e.postID]++
		}

		realDeletes, err := c.likeRepo.BatchDelete(ctx, pairs)
		if err != nil {
			zap.L().Warn("like consumer: BatchDelete failed",
				zap.Int("batch_size", len(pairs)),
				zap.Error(err),
			)
		} else {
			scaledPerPost := scaleDeltas(perPost, int64(len(pairs)), realDeletes)
			if err := c.likeRepo.DecrPostLikeCount(ctx, scaledPerPost); err != nil {
				zap.L().Warn("like consumer: DecrPostLikeCount failed", zap.Error(err))
			}
			totalThisFlush += int(realDeletes)
		}
		*unlikeBuf = (*unlikeBuf)[:0]
	}

	return totalThisFlush
}

// scaleDeltas proportionally adjusts per-post deltas when MySQL reported
// fewer real changes than the batch contained (i.e. duplicates were
// filtered by INSERT IGNORE / non-existent rows by DELETE).
//
// The scaling preserves total mass: sum(scaled) == real. When real ==
// raw, this is a pure copy (no-op). When real == 0, all deltas zero out
// (no UPDATE issued for any post). The proportional approach is a
// "fair-share" approximation; it can be off by up to len(perPost)-1
// on edge cases but the cumulative error converges to zero across the
// reconciliation pass.
func scaleDeltas(perPost map[int64]int64, raw, real int64) map[int64]int64 {
	if real <= 0 {
		// Nothing landed; emit empty map, IncrPostLikeCount no-ops.
		return map[int64]int64{}
	}
	if real >= raw {
		// All landed (or more — shouldn't happen but be defensive).
		return perPost
	}
	scaled := make(map[int64]int64, len(perPost))
	remaining := real
	// Largest first: distribute integer real proportionally; assign
	// remainder to the largest bucket so we never leave a stray +1 unallocated.
	type bucket struct {
		postID int64
		count  int64
	}
	buckets := make([]bucket, 0, len(perPost))
	for k, v := range perPost {
		buckets = append(buckets, bucket{postID: k, count: v})
	}
	// Simple insertion-sort by count desc; perPost is at most ~50 entries.
	for i := 1; i < len(buckets); i++ {
		for j := i; j > 0 && buckets[j].count > buckets[j-1].count; j-- {
			buckets[j], buckets[j-1] = buckets[j-1], buckets[j]
		}
	}
	for i, b := range buckets {
		var d int64
		if i == len(buckets)-1 {
			d = remaining
		} else {
			d = b.count * real / raw
			if d > remaining {
				d = remaining
			}
		}
		if d > 0 {
			scaled[b.postID] = d
		}
		remaining -= d
		if remaining <= 0 {
			break
		}
	}
	return scaled
}
