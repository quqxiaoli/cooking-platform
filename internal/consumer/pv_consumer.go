// Package consumer — pv_consumer.go consumes TopicPV events emitted by
// PostService.GetDetail() and persists deduplicated view counts to the
// posts.view_count column.
//
// ── Why batch 100 / 5s ──────────────────────────────────────────────────────
//
// View events are the highest-rate event in the system: every detail-page
// load that passes Redis dedup emits one. PRD-Phase3 §3.4 sets the batch
// at 100 events or 5 seconds. The reasoning:
//
//   - PV is the lowest-business-impact write (a stale view_count by one
//     batch interval has zero user-visible consequence). Larger batches
//     amortise UPDATE overhead at no real cost.
//   - 5 seconds is a reasonable upper bound on staleness for analytics
//     dashboards (Step 16 Grafana panels read view_count).
//   - 100 events × ~50 distinct hot posts → ~50 UPDATEs per flush, well
//     within MySQL's comfort zone.
//
// ── Why GROUP BY post_id (delta SQL only) ──────────────────────────────────
//
// PVConsumer aggregates events by post_id, computes a per-post +N delta,
// and issues `UPDATE posts SET view_count = view_count + ?` per post.
// The instruction is explicit on this: increment SQL only, NEVER set
// absolute values. Reasoning:
//
//   - Multiple Go instances (Step 15 Nginx LB) all flush concurrently.
//     SET absolute would race: instance A reads view_count=100, computes
//     100+5=105, instance B reads 100, computes 100+3=103, last-writer
//     wins → 8 views lost. UPDATE +N is commutative; both writes are
//     correctly applied in any order.
//   - Even single-instance MVP benefits: if the consumer crashes between
//     SELECT and UPDATE in a SET-absolute design, view_count regresses.
//     Increment-only never regresses.
//
// ── Graceful shutdown ──────────────────────────────────────────────────────
//
// Mirrors LikeConsumer's drain pattern: ctx cancel → wait for subscribe
// goroutine → drain remaining buffer to MySQL via context.Background().
//
// Added in Step 5 (like module — but PVConsumer is a Step-4 deferred dep
// that we land here so view_count finally moves off zero).
package consumer

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"cooking-platform/internal/event"
	"cooking-platform/pkg/config"

	"go.uber.org/zap"
	"gorm.io/gorm"
)

// PVConsumer subscribes to TopicPV and batches view increments.
type PVConsumer struct {
	bus           event.EventSubscriber
	db            *gorm.DB
	batchSize     int
	flushInterval time.Duration
}

// NewPVConsumer constructs a PVConsumer.
// cfg provides batch/flush knobs previously hardcoded as package-level constants.
func NewPVConsumer(bus event.EventSubscriber, db *gorm.DB, cfg config.PVConsumerConfig) *PVConsumer {
	return &PVConsumer{
		bus:           bus,
		db:            db,
		batchSize:     cfg.BatchSize,
		flushInterval: cfg.FlushInterval,
	}
}

// Name satisfies EventConsumer.
func (c *PVConsumer) Name() string {
	return "pv-consumer"
}

// Start blocks until ctx is cancelled.
func (c *PVConsumer) Start(ctx context.Context) error {
	eventCh := make(chan int64, c.batchSize*4) // post_id directly; no need for full struct

	var subWg sync.WaitGroup
	subWg.Add(1)
	go func() {
		defer subWg.Done()
		_ = c.bus.Subscribe(ctx, event.TopicPV, func(_ context.Context, payload []byte) error {
			var p event.PVEvent
			if err := json.Unmarshal(payload, &p); err != nil {
				zap.L().Warn("pv consumer: unmarshal PVEvent", zap.Error(err))
				return nil
			}
			select {
			case eventCh <- p.PostID:
			case <-ctx.Done():
				return nil
			}
			return nil
		})
	}()

	c.flushLoop(ctx, eventCh, &subWg)
	return nil
}

// flushLoop accumulates per-post counts and flushes on cap or tick.
func (c *PVConsumer) flushLoop(ctx context.Context, eventCh chan int64, subWg *sync.WaitGroup) {
	deltas := make(map[int64]int64, c.batchSize)
	bufCount := 0

	ticker := time.NewTicker(c.flushInterval)
	defer ticker.Stop()

	totalProcessed := 0

	for {
		select {
		case postID := <-eventCh:
			deltas[postID]++
			bufCount++
			if bufCount >= c.batchSize {
				totalProcessed += c.flush(ctx, deltas)
				deltas = make(map[int64]int64, c.batchSize)
				bufCount = 0
			}

		case <-ticker.C:
			if bufCount > 0 {
				totalProcessed += c.flush(ctx, deltas)
				deltas = make(map[int64]int64, c.batchSize)
				bufCount = 0
			}

		case <-ctx.Done():
			subWg.Wait()
			DrainChan(eventCh, func(postID int64) {
				deltas[postID]++
				bufCount++
			})
			if bufCount > 0 {
				totalProcessed += c.flush(context.Background(), deltas)
			}
			zap.L().Info("pv consumer drained",
				zap.Int("total_processed", totalProcessed),
			)
			return
		}
	}
}

// flush issues one increment-UPDATE per distinct post_id in the batch.
// Returns the total events processed (sum of deltas) for logging.
func (c *PVConsumer) flush(ctx context.Context, deltas map[int64]int64) int {
	if len(deltas) == 0 {
		return 0
	}
	total := 0
	for postID, delta := range deltas {
		if delta <= 0 {
			continue
		}
		err := c.db.WithContext(ctx).
			Exec("UPDATE posts SET view_count = view_count + ? WHERE id = ?", delta, postID).
			Error
		if err != nil {
			zap.L().Warn("pv consumer: UPDATE view_count failed",
				zap.Int64("post_id", postID),
				zap.Int64("delta", delta),
				zap.Error(err),
			)
			continue
		}
		total += int(delta)
	}
	return total
}
