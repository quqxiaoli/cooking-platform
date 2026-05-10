// Package service — like_service.go orchestrates the like / unlike business
// flow, composing the post repository, the like cache, and the event publisher.
//
// ── Hot-path ordering ──────────────────────────────────────────────────────
//
// Every Like / Unlike call follows the same template:
//
//  1. Validate post exists and is fetchable (one MySQL read; cheap because
//     posts are cached at the page level — Step 4's feed cache — and the
//     detail itself is hit-rate-friendly). Bail with ErrPostNotFound if
//     missing. We need post.UserID anyway so the published event can carry
//     AuthorID for CountConsumer to update users.total_likes.
//
//  2. SISMEMBER probe for idempotency. If the user has already liked
//     (or already not-liked, on Unlike), short-circuit: return the
//     current count without further work. NO event is published in
//     this branch — this is the cornerstone of "MQ messages reflect
//     real state changes, not user-visible API calls".
//
//  3. SADD + INCR (or SREM + DECR-clamp). User-visible state flips here;
//     the Redis ops are the source of truth for the synchronous response.
//
//  4. Publish LikeEvent / UnlikeEvent. Best-effort: a publish failure
//     logs a warning but does NOT roll back step 3 — the user has
//     already seen "liked". The eventual MySQL write will lag by one
//     reconciliation pass at most. This is the standard "cache-first,
//     MQ for durability" tradeoff our PRD-Phase3 §4 commits to.
//
// ── Why not transactional outbox in MVP ────────────────────────────────────
//
// Strict consistency between "Redis write succeeded" and "MQ message
// persisted" requires a transactional outbox: write an "outbox" row in the
// same MySQL transaction as the business change, then a relay process
// reads outbox → publishes → marks delivered. That adds an outbox table,
// a relay consumer, idempotency on the consumer side, and a poll cadence.
//
// Channel-mode MVP doesn't need it: the MQ is in-process, publish failure
// only happens at shutdown, and our shutdown order (HTTP → consumers →
// bus) ensures no in-flight publishes are dropped under normal conditions.
//
// Step 13 (RabbitMQ switch) will revisit this; for now the simpler "publish
// after Redis, log on failure" is correct.
//
// ── Why not return MySQL like_count to the user ────────────────────────────
//
// posts.like_count lags reality by up to 3 seconds (LikeConsumer batch
// flush). The user just clicked the heart — showing them the lagging
// number causes a "did my click register?" UX bug. We return the Redis
// counter (post-INCR) which is fresh by definition. MySQL stays the
// long-term source of truth for cold-cache reads (Step 9+ when feed
// cards display like_count without consulting Redis).
//
// Added in Step 5 (like module).
package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"cooking-platform/internal/cache"
	"cooking-platform/internal/event"
	"cooking-platform/internal/model/dto"
	"cooking-platform/internal/repository"
	"cooking-platform/pkg/errcode"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

// LikeService is the entry point for like-module business operations.
type LikeService struct {
	postRepo  repository.PostRepository
	likeCache *cache.LikeCache
	bus       event.EventPublisher
}

// NewLikeService wires the service with its dependencies.
//
// Note we take EventPublisher (not the full EventBus): LikeService only
// publishes, never subscribes. Subscription lives in LikeConsumer. The
// narrower interface makes it obvious at the type level that this layer
// can never accidentally drain events meant for a consumer.
func NewLikeService(
	postRepo repository.PostRepository,
	likeCache *cache.LikeCache,
	bus event.EventPublisher,
) *LikeService {
	return &LikeService{
		postRepo:  postRepo,
		likeCache: likeCache,
		bus:       bus,
	}
}

// ── Public API ──────────────────────────────────────────────────────────────

// Like registers a like by userID on postID.
//
// Idempotent: re-liking returns liked=true with the current count and does
// NOT republish an event. Self-likes (userID == post.UserID) are permitted —
// see Step 5 PRD deviation log; mainstream platforms allow this.
//
// Returns ErrPostNotFound if the post doesn't exist or has been soft-deleted.
// Other errors are wrapped infrastructure errors → 500.
func (s *LikeService) Like(ctx context.Context, userID, postID int64) (*dto.LikeResp, error) {
	post, err := s.postRepo.FindByID(ctx, postID)
	if err != nil {
		if errors.Is(err, repository.ErrPostNotFound) {
			return nil, errcode.ErrPostNotFound
		}
		return nil, fmt.Errorf("find post for like: %w", err)
	}
	authorID := post.UserID

	// Idempotency probe.
	already, err := s.likeCache.HasLiked(ctx, postID, userID)
	if err != nil {
		return nil, fmt.Errorf("check liked: %w", err)
	}
	if already {
		// User already liked. Return current count without re-publishing.
		count, err := s.likeCache.GetLikeCount(ctx, postID)
		if err != nil {
			return nil, fmt.Errorf("get like count (idempotent path): %w", err)
		}
		return &dto.LikeResp{Liked: true, Count: count}, nil
	}

	// Real state change: SADD + INCR.
	count, err := s.likeCache.AddLike(ctx, postID, userID)
	if err != nil {
		return nil, fmt.Errorf("add like: %w", err)
	}

	// Publish LikeEvent. Best-effort — a failed publish doesn't roll back
	// the user-visible state change. See file header for the tradeoff.
	s.publishLikeEvent(ctx, userID, postID, authorID)

	return &dto.LikeResp{Liked: true, Count: count}, nil
}

// Unlike removes the like by userID on postID.
//
// Idempotent: unliking when not currently liked returns liked=false with
// the current count and does NOT republish an event.
//
// Returns ErrPostNotFound if the post doesn't exist.
func (s *LikeService) Unlike(ctx context.Context, userID, postID int64) (*dto.LikeResp, error) {
	post, err := s.postRepo.FindByID(ctx, postID)
	if err != nil {
		if errors.Is(err, repository.ErrPostNotFound) {
			return nil, errcode.ErrPostNotFound
		}
		return nil, fmt.Errorf("find post for unlike: %w", err)
	}
	authorID := post.UserID

	already, err := s.likeCache.HasLiked(ctx, postID, userID)
	if err != nil {
		return nil, fmt.Errorf("check liked: %w", err)
	}
	if !already {
		// User has not liked. Return current count without re-publishing.
		count, err := s.likeCache.GetLikeCount(ctx, postID)
		if err != nil {
			return nil, fmt.Errorf("get like count (idempotent path): %w", err)
		}
		return &dto.LikeResp{Liked: false, Count: count}, nil
	}

	count, err := s.likeCache.RemoveLike(ctx, postID, userID)
	if err != nil {
		return nil, fmt.Errorf("remove like: %w", err)
	}

	s.publishUnlikeEvent(ctx, userID, postID, authorID)

	return &dto.LikeResp{Liked: false, Count: count}, nil
}

// GetLikeStatus returns the current like state of userID on postID.
//
// Used by GET /api/v1/posts/:id/like to let a returning user see whether
// their previous like is still recorded — typical front-end use is to paint
// the heart filled or empty when rendering a detail page.
//
// userID==0 (unauthenticated) is rejected upstream by the Auth middleware,
// so this method always sees a real user.
func (s *LikeService) GetLikeStatus(ctx context.Context, userID, postID int64) (*dto.LikeResp, error) {
	// Validate post exists. We could skip this and just return whatever
	// Redis says, but reporting "you have not liked a non-existent post"
	// is misleading; ErrPostNotFound is the honest answer.
	if _, err := s.postRepo.FindByID(ctx, postID); err != nil {
		if errors.Is(err, repository.ErrPostNotFound) {
			return nil, errcode.ErrPostNotFound
		}
		return nil, fmt.Errorf("find post for get-like-status: %w", err)
	}

	liked, err := s.likeCache.HasLiked(ctx, postID, userID)
	if err != nil {
		return nil, fmt.Errorf("check liked: %w", err)
	}
	count, err := s.likeCache.GetLikeCount(ctx, postID)
	if err != nil {
		return nil, fmt.Errorf("get like count: %w", err)
	}
	return &dto.LikeResp{Liked: liked, Count: count}, nil
}

// ── Private helpers ─────────────────────────────────────────────────────────

// publishLikeEvent emits a LikeEvent on TopicLike. Failure logs a warn and
// is swallowed — see file header for the design tradeoff.
//
// EventID and outer Event.ID are deliberately the same value so consumers
// have a single canonical identifier for idempotency / log correlation,
// regardless of whether they unmarshal the envelope or the payload first.
func (s *LikeService) publishLikeEvent(ctx context.Context, userID, postID, authorID int64) {
	now := time.Now().UnixMilli()
	id := uuid.NewString()

	payload := event.LikeEvent{
		EventID:   id,
		UserID:    userID,
		PostID:    postID,
		AuthorID:  authorID,
		Timestamp: now,
	}
	evt := event.Event{
		ID:        id,
		Topic:     event.TopicLike,
		Timestamp: now,
		Payload:   event.MustMarshalPayload(payload),
	}
	if err := s.bus.Publish(ctx, evt); err != nil {
		zap.L().Warn("publish like event failed",
			zap.Int64("post_id", postID),
			zap.Int64("user_id", userID),
			zap.Error(err),
		)
	}
}

// publishUnlikeEvent emits an UnlikeEvent on TopicUnlike. Symmetric to
// publishLikeEvent.
func (s *LikeService) publishUnlikeEvent(ctx context.Context, userID, postID, authorID int64) {
	now := time.Now().UnixMilli()
	id := uuid.NewString()

	payload := event.UnlikeEvent{
		EventID:   id,
		UserID:    userID,
		PostID:    postID,
		AuthorID:  authorID,
		Timestamp: now,
	}
	evt := event.Event{
		ID:        id,
		Topic:     event.TopicUnlike,
		Timestamp: now,
		Payload:   event.MustMarshalPayload(payload),
	}
	if err := s.bus.Publish(ctx, evt); err != nil {
		zap.L().Warn("publish unlike event failed",
			zap.Int64("post_id", postID),
			zap.Int64("user_id", userID),
			zap.Error(err),
		)
	}
}
