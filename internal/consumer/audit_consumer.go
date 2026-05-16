// Package consumer — audit_consumer.go subscribes to TopicAudit and drives
// the content-moderation pipeline for every new post.
//
// ── Flow (one-hop, PRD §9.2) ─────────────────────────────────────────────────
//
//  PostService.Create
//    └─ Publish AuditEvent{AuditStatus=0} → TopicAudit
//         └─ AuditConsumer.Start goroutine
//              ├─ postRepo.FindByID + LoadSteps   (fetch reviewable content)
//              ├─ pkg/audit.Auditor.Review()      (call Aliyun Green or mock)
//              ├─ auditRepo.Create()              (append audit_log row)
//              ├─ postRepo.UpdateAuditStatus()    (flip audit_status + is_visible)
//              └─ feedCache.BumpFeedVersion()     (invalidate feed cache)
//
// ── Why no batching ──────────────────────────────────────────────────────────
//
// Unlike LikeConsumer (50-event batches) or PVConsumer (100-event batches),
// AuditConsumer processes events one-by-one. Reasons:
//
//  1. Publish rate is low: each post triggers exactly one AuditEvent. Even a
//     popular platform rarely exceeds a few events/second on a single node.
//  2. The Aliyun Green API is synchronous with variable latency (~200-800 ms).
//     Batching would require holding all events in memory while waiting for N
//     API round-trips — no throughput gain, added complexity.
//  3. Each audit result must update its own post row immediately: delaying
//     visibility by "wait for 50 events" would harm UX.
//
// ── Degradation ──────────────────────────────────────────────────────────────
//
// If the content safety API fails (network timeout, quota exceeded), the
// consumer logs the error and DOES NOT update the post. The post remains
// in is_visible=0 / audit_status=0 (pending). A future reconcile job or
// manual admin action can requeue it. We never flip is_visible=1 on an
// API error — fail-closed is the correct default for content safety.
//
// ── Graceful shutdown ────────────────────────────────────────────────────────
//
// ctx cancellation (from ConsumerManager.Shutdown) causes the Subscribe
// goroutine to exit. Any in-flight Auditor.Review call completes because
// it holds the pre-cancel ctx (not the shutdown ctx). This mirrors the
// LikeConsumer drain pattern: "finish what's in flight, don't start new."
//
// Added in Step 10 (content moderation).
package consumer

import (
	"context"
	"encoding/json"
	"time"

	"cooking-platform/internal/cache"
	"cooking-platform/internal/event"
	"cooking-platform/internal/model"
	"cooking-platform/internal/repository"
	"cooking-platform/pkg/audit"

	"go.uber.org/zap"
)

// AuditConsumer subscribes to TopicAudit, calls the content-safety provider,
// and persists the verdict to posts + audit_logs.
type AuditConsumer struct {
	bus       event.EventBus
	postRepo  repository.PostRepository
	auditRepo repository.AuditRepository
	auditor   audit.Auditor
	feedCache *cache.FeedCache
}

// NewAuditConsumer constructs an AuditConsumer.
func NewAuditConsumer(
	bus event.EventBus,
	postRepo repository.PostRepository,
	auditRepo repository.AuditRepository,
	auditor audit.Auditor,
	feedCache *cache.FeedCache,
) *AuditConsumer {
	return &AuditConsumer{
		bus:       bus,
		postRepo:  postRepo,
		auditRepo: auditRepo,
		auditor:   auditor,
		feedCache: feedCache,
	}
}

// Name satisfies the EventConsumer interface. Used in lifecycle log lines.
func (c *AuditConsumer) Name() string {
	return "audit-consumer"
}

// Start blocks until ctx is cancelled, processing one AuditEvent at a time.
// Each event triggers a synchronous content-safety API call followed by DB
// writes. Returns nil — ConsumerManager's wg.Wait() unblocks on return.
func (c *AuditConsumer) Start(ctx context.Context) error {
	zap.L().Info("audit consumer started")

	_ = c.bus.Subscribe(ctx, event.TopicAudit, func(subCtx context.Context, payload []byte) error {
		var e event.AuditEvent
		if err := json.Unmarshal(payload, &e); err != nil {
			zap.L().Warn("audit consumer: unmarshal AuditEvent failed",
				zap.Error(err),
				zap.ByteString("payload", payload),
			)
			return nil // bad payload — don't retry
		}

		// Only process submission events (AuditStatus=0 = pending).
		// Non-zero status would mean a result already arrived via another path
		// (e.g. future manual-review admin API). Skip to avoid double-write.
		if e.AuditStatus != int8(model.AuditStatusPending) {
			return nil
		}

		c.process(subCtx, e)
		return nil
	})

	zap.L().Info("audit consumer stopped")
	return nil
}

// process runs the full audit pipeline for one submission event.
// Errors at any step are logged and do not propagate — the consumer must
// stay alive for subsequent events even if one post fails.
func (c *AuditConsumer) process(ctx context.Context, e event.AuditEvent) {
	log := zap.L().With(zap.Int64("post_id", e.PostID))

	// Load post to get title + content.
	post, err := c.postRepo.FindByID(ctx, e.PostID)
	if err != nil {
		log.Warn("audit consumer: load post failed", zap.Error(err))
		return
	}

	// Load steps to collect all image URLs for image scan.
	steps, err := c.postRepo.LoadSteps(ctx, e.PostID)
	if err != nil {
		log.Warn("audit consumer: load steps failed; proceeding with text-only review",
			zap.Error(err),
		)
		steps = nil
	}

	imageURLs := collectImageURLs(post, steps)

	// Call content safety provider (blocking ~200-800 ms for Aliyun, ~0 ms for mock).
	result, err := c.auditor.Review(ctx, audit.ReviewRequest{
		PostID:    e.PostID,
		AuthorID:  e.AuthorID,
		Title:     post.Title,
		Content:   post.Content,
		ImageURLs: imageURLs,
	})
	if err != nil {
		// Fail-closed: leave post invisible, log error for alerting.
		log.Error("audit consumer: review API failed; post remains pending",
			zap.Error(err),
			zap.String("errcode", "470101"),
		)
		return
	}

	// Determine is_visible from verdict.
	isVisible := model.PostInvisible
	if result.Status == model.AuditStatusMachinePass {
		isVisible = model.PostVisible
	}

	// Append audit_log row first (compliance trail, no rollback needed if
	// the subsequent UpdateAuditStatus fails — the log row is append-only).
	auditLog := &model.AuditLog{
		PostID:      e.PostID,
		AuthorID:    e.AuthorID,
		AuditStatus: result.Status,
		Remark:      result.Remark,
		RawResponse: result.Raw,
		CreatedAt:   time.Now(),
	}
	if err := c.auditRepo.Create(ctx, auditLog); err != nil {
		log.Error("audit consumer: write audit_log failed",
			zap.Error(err),
			zap.String("errcode", "470102"),
		)
		// Do NOT return — still try to update posts so the post isn't stuck.
	}

	// Update posts.audit_status + is_visible atomically in one UPDATE.
	if err := c.postRepo.UpdateAuditStatus(ctx, e.PostID, result.Status, isVisible, result.Remark); err != nil {
		log.Error("audit consumer: UpdateAuditStatus failed",
			zap.Error(err),
			zap.String("errcode", "470102"),
		)
		return
	}

	log.Info("audit consumer: post reviewed",
		zap.Uint8("audit_status", result.Status),
		zap.Uint8("is_visible", isVisible),
	)

	// Bump feed version so the feed cache is invalidated. Approved posts
	// should appear in the feed immediately; rejected posts caused no cache
	// entry anyway (is_visible=0 rows are excluded from ListFeed).
	if isVisible == model.PostVisible {
		if _, err := c.feedCache.BumpFeedVersion(ctx); err != nil {
			log.Warn("audit consumer: BumpFeedVersion failed", zap.Error(err))
		}
	}
}

// collectImageURLs gathers the post's cover URL + all step image URLs into
// a flat slice for the image scan task. Empty strings (no cover) are skipped.
func collectImageURLs(post *model.Post, steps []*model.PostStep) []string {
	var urls []string
	if post.CoverURL != "" {
		urls = append(urls, post.CoverURL)
	}
	for _, s := range steps {
		urls = append(urls, []string(s.ImageURLs)...)
	}
	return urls
}
