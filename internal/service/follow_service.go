// Package service — follow_service.go is the business-logic layer for the
// follow module. It orchestrates the FollowRepository (relationship I/O),
// the UserRepository (target-existence check), and the EventPublisher
// (async counter maintenance).
//
// Boundary rules (identical to user_service.go):
//   - Returns *errcode.AppError for expected business failures; the handler
//     converts these to HTTP via response.FromError.
//   - Returns a wrapped error for unexpected infrastructure failures; the
//     handler maps those to 500.
//   - ctx is threaded through every repository / publisher call.
//
// ── Why follows are written synchronously, but counts asynchronously ───────
//
// The follows-table write (INSERT IGNORE / DELETE) happens inline, on the
// request goroutine. The redundant users.follower_count / following_count
// updates are deferred to CountConsumer via FollowEvent / UnfollowEvent.
//
// This split is deliberate (Step 8 ADR / 故事线):
//
//   - Follow is low-frequency (a user follows someone once, not 200×/day
//     like a like). There is no write-amplification problem to batch away,
//     so the like module's "Redis SET + async LikeConsumer" machinery would
//     be pure overhead here.
//   - A synchronous follows write means the relationship is durable and
//     queryable the instant the API returns — no eventual-consistency
//     window where "did my follow stick?" is ambiguous. The follows table
//     is the source of truth for the relationship.
//   - The ONLY thing that tolerates lag is the *display counter*
//     (follower_count on a profile). That is exactly what CountConsumer
//     already does for post_count — so follow reuses it rather than
//     inventing a parallel path.
//
// ── Idempotency ────────────────────────────────────────────────────────────
//
// Follow: re-following someone you already follow returns following=true
// with no event published and no counter change (AC-4 "已关注再次点击 →
// 已关注状态"). The Exists() fast-path handles the common case; INSERT
// IGNORE's RowsAffected=0 is the race-safe backstop if two concurrent
// follows slip past Exists().
//
// Unfollow: unfollowing someone you do NOT follow is an error
// (ErrFollowNotFound, 440103) — per the Step 8 contract, which deliberately
// chose explicit-error over silent-idempotent for the unfollow direction.
//
// Added in Step 8 (follow module).
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"cooking-platform/internal/event"
	"cooking-platform/internal/model/dto"
	"cooking-platform/internal/repository"
	"cooking-platform/pkg/config"
	"cooking-platform/pkg/errcode"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

// FollowService is the entry point for all follow-related business operations.
//
// maxFollowing / defaultFollowListSize / maxFollowListSize were promoted from
// package consts to cfg.Follow in Step 18 (FOLLOW-01, Config-First). Stored
// on the struct so the helper buildFollowListResp / clamp can read them.
type FollowService struct {
	followRepo      repository.FollowRepository
	userRepo        repository.UserRepository
	bus             event.EventPublisher
	maxFollowing    int
	defaultListSize int
	maxListSize     int
}

// NewFollowService wires the service with its dependencies.
//
// userRepo is the same shared instance the other services use — needed only
// to verify the follow target exists (mapping a missing target to the
// canonical ErrUserNotFound / 410108 rather than minting a follow-module
// duplicate).
//
// bus is the EventPublisher interface, not the full EventBus: FollowService
// emits events, it never subscribes — subscription belongs to CountConsumer,
// wired separately in main.go stage 7.6.
func NewFollowService(
	followRepo repository.FollowRepository,
	userRepo repository.UserRepository,
	bus event.EventPublisher,
	cfg config.FollowConfig,
) *FollowService {
	return &FollowService{
		followRepo:      followRepo,
		userRepo:        userRepo,
		bus:             bus,
		maxFollowing:    cfg.MaxFollowing,
		defaultListSize: cfg.DefaultListSize,
		maxListSize:     cfg.MaxListSize,
	}
}

// ── Public API ──────────────────────────────────────────────────────────────

// Follow makes followerID follow targetID.
//
// Check order is intentional — cheapest / most-specific first:
//  1. self-follow      → 440101 (pure comparison, no I/O)
//  2. target exists    → 410108 (one indexed SELECT)
//  3. already following → idempotent success, no cap check, no event
//  4. 3000-follow cap  → 440102 (counted from the follows table)
//  5. INSERT IGNORE    → publish FollowEvent only if a row was really written
//
// Returning following=true in step 3 without touching the cap is correct:
// an existing relationship must never be rejected by a cap that itself
// already counts that relationship.
func (s *FollowService) Follow(ctx context.Context, followerID, targetID int64) (*dto.FollowActionResp, error) {
	// 1. Cannot follow yourself (AC-3). Interface-layer rejection, before any I/O.
	if followerID == targetID {
		return nil, errcode.ErrCannotFollowSelf
	}

	// 2. Target must exist. Reuse the canonical user-not-found code (410108)
	//    rather than minting a follow-module duplicate.
	if _, err := s.userRepo.FindByID(ctx, targetID); err != nil {
		if errors.Is(err, repository.ErrUserNotFound) {
			return nil, errcode.ErrUserNotFound
		}
		return nil, fmt.Errorf("follow: find target: %w", err)
	}

	// 3. Idempotent fast path: already following → success, no event.
	already, err := s.followRepo.Exists(ctx, followerID, targetID)
	if err != nil {
		return nil, fmt.Errorf("follow: exists check: %w", err)
	}
	if already {
		return &dto.FollowActionResp{Following: true}, nil
	}

	// 4. Enforce the 3000-follow cap (AC-5). Counted from the follows table
	//    (source of truth), not the lagging users.following_count.
	cnt, err := s.followRepo.CountFollowing(ctx, followerID)
	if err != nil {
		return nil, fmt.Errorf("follow: count following: %w", err)
	}
	if cnt >= int64(s.maxFollowing) {
		return nil, errcode.ErrFollowLimitExceeded
	}

	// 5. Insert the edge. INSERT IGNORE is race-safe: if a concurrent request
	//    created the same edge between step 3 and here, inserted=false and we
	//    skip the event — no double counting.
	inserted, err := s.followRepo.Create(ctx, followerID, targetID)
	if err != nil {
		return nil, fmt.Errorf("follow: create: %w", err)
	}
	if inserted {
		s.publishFollowEvent(ctx, followerID, targetID)
	}

	return &dto.FollowActionResp{Following: true}, nil
}

// Unfollow makes followerID stop following targetID.
//
// Unlike Follow, there is no self-check branch: a self-follow edge can never
// exist (Follow rejects it at step 1), so Delete simply finds nothing and
// returns deleted=false → ErrFollowNotFound, which is the honest answer.
//
// deleted=false → 440103: per the Step 8 contract, unfollowing a
// non-existent relationship is an explicit error, not a silent no-op.
func (s *FollowService) Unfollow(ctx context.Context, followerID, targetID int64) (*dto.FollowActionResp, error) {
	deleted, err := s.followRepo.Delete(ctx, followerID, targetID)
	if err != nil {
		return nil, fmt.Errorf("unfollow: delete: %w", err)
	}
	if !deleted {
		return nil, errcode.ErrFollowNotFound
	}

	s.publishUnfollowEvent(ctx, followerID, targetID)
	return &dto.FollowActionResp{Following: false}, nil
}

// ListFollowers returns a cursor-paginated page of the users who follow
// targetID (the people on targetID's "粉丝" list).
//
// Public endpoint — no auth (anyone can view a profile's follower list,
// consistent with GET /users/:id/posts being public). targetID is NOT
// existence-checked: a non-existent user simply has zero followers, and a
// 200-with-empty-list is friendlier than a 404 for a list endpoint.
func (s *FollowService) ListFollowers(ctx context.Context, targetID int64, cursor string, size int) (*dto.FollowListResp, error) {
	cursorFollowID, err := parseFollowCursor(cursor)
	if err != nil {
		return nil, err
	}
	size = s.clampFollowListSize(size)

	// Fetch size+1 to detect has_more without a separate COUNT query —
	// the same trick the feed module uses.
	rows, err := s.followRepo.ListFollowers(ctx, targetID, cursorFollowID, size+1)
	if err != nil {
		return nil, fmt.Errorf("list followers: %w", err)
	}
	return buildFollowListResp(rows, size), nil
}

// ListFollowing returns a cursor-paginated page of the users targetID
// follows (targetID's "关注" list). Same contract as ListFollowers.
func (s *FollowService) ListFollowing(ctx context.Context, targetID int64, cursor string, size int) (*dto.FollowListResp, error) {
	cursorFollowID, err := parseFollowCursor(cursor)
	if err != nil {
		return nil, err
	}
	size = s.clampFollowListSize(size)

	rows, err := s.followRepo.ListFollowing(ctx, targetID, cursorFollowID, size+1)
	if err != nil {
		return nil, fmt.Errorf("list following: %w", err)
	}
	return buildFollowListResp(rows, size), nil
}

// ── Event publishing ────────────────────────────────────────────────────────

// publishFollowEvent emits a FollowEvent on TopicFollow. CountConsumer
// consumes it and applies +1 to following_count (the follower) and +1 to
// follower_count (the followee).
//
// Publish failures are logged, not propagated: the follow relationship is
// already durably written to MySQL (the source of truth). A lost event only
// means the redundant display counter drifts — a periodic reconcile job
// (post-MVP) realigns it. Failing the whole request because a best-effort
// counter update could not be queued would be the wrong trade-off, and is
// consistent with how PostService treats its PostEvent publish.
func (s *FollowService) publishFollowEvent(ctx context.Context, followerID, followingID int64) {
	id := uuid.NewString()
	now := time.Now()

	payload, err := json.Marshal(event.FollowEvent{
		EventID:     id,
		FollowerID:  followerID,
		FollowingID: followingID,
		Timestamp:   now.UnixMilli(),
	})
	if err != nil {
		zap.L().Warn("follow service: marshal FollowEvent",
			zap.Int64("follower_id", followerID),
			zap.Int64("following_id", followingID),
			zap.Error(err),
		)
		return
	}

	if err := s.bus.Publish(ctx, event.Event{
		ID:        id,
		Topic:     event.TopicFollow,
		Timestamp: now.UnixMilli(),
		Payload:   payload,
	}); err != nil {
		zap.L().Warn("follow service: publish FollowEvent",
			zap.Int64("follower_id", followerID),
			zap.Int64("following_id", followingID),
			zap.Error(err),
		)
	}
}

// publishUnfollowEvent emits an UnfollowEvent on TopicUnfollow. CountConsumer
// applies -1 to both following_count (follower) and follower_count (followee),
// clamped at 0. Same best-effort logging policy as publishFollowEvent.
func (s *FollowService) publishUnfollowEvent(ctx context.Context, followerID, followingID int64) {
	id := uuid.NewString()
	now := time.Now()

	payload, err := json.Marshal(event.UnfollowEvent{
		EventID:     id,
		FollowerID:  followerID,
		FollowingID: followingID,
		Timestamp:   now.UnixMilli(),
	})
	if err != nil {
		zap.L().Warn("follow service: marshal UnfollowEvent",
			zap.Int64("follower_id", followerID),
			zap.Int64("following_id", followingID),
			zap.Error(err),
		)
		return
	}

	if err := s.bus.Publish(ctx, event.Event{
		ID:        id,
		Topic:     event.TopicUnfollow,
		Timestamp: now.UnixMilli(),
		Payload:   payload,
	}); err != nil {
		zap.L().Warn("follow service: publish UnfollowEvent",
			zap.Int64("follower_id", followerID),
			zap.Int64("following_id", followingID),
			zap.Error(err),
		)
	}
}

// ── Private helpers ─────────────────────────────────────────────────────────

// parseFollowCursor decodes the opaque cursor string into a follows.id
// keyset boundary.
//
//   - ""  → 0  → first page (the repository applies no `f.id < ?` predicate).
//   - "N" → N  → return follows rows with id < N.
//
// A non-empty, non-numeric, or non-positive cursor is a client error
// (ErrFollowCursorInvalid, 440104) — the cursor is server-issued and opaque,
// so a malformed one means tampering or a client bug, not a recoverable state.
func parseFollowCursor(cursor string) (int64, error) {
	if cursor == "" {
		return 0, nil
	}
	id, err := strconv.ParseInt(cursor, 10, 64)
	if err != nil || id <= 0 {
		return 0, errcode.ErrFollowCursorInvalid
	}
	return id, nil
}

// clampFollowListSize bounds the requested page size to [1, s.maxListSize],
// defaulting to s.defaultListSize when the caller passes 0 (param absent).
// Mirrors PRD-Phase3 §5.4; bounds were promoted from package consts to
// cfg.Follow in Step 18 (FOLLOW-01) so deployments can tune them without
// recompiling.
func (s *FollowService) clampFollowListSize(size int) int {
	if size <= 0 {
		return s.defaultListSize
	}
	if size > s.maxListSize {
		return s.maxListSize
	}
	return size
}

// buildFollowListResp turns the size+1 over-fetch into a wire response.
//
// has_more logic: the repository was asked for size+1 rows. If it returned
// more than `size`, there is at least one further page; we trim to `size`
// and set next_cursor to the last *kept* row's follow_id. If it returned
// `size` or fewer, this is the final page — next_cursor is "" and
// has_more=false.
//
// next_cursor carries follows.id (decimal string) — opaque to the client,
// passed back verbatim as the `cursor` query param. Same opaque-cursor
// contract as the feed module.
func buildFollowListResp(rows []repository.FollowUser, size int) *dto.FollowListResp {
	hasMore := len(rows) > size
	if hasMore {
		rows = rows[:size]
	}

	users := make([]dto.UserBrief, 0, len(rows))
	for _, r := range rows {
		users = append(users, dto.UserBrief{
			ID:        r.UserID,
			Nickname:  r.Nickname,
			AvatarURL: r.AvatarURL,
		})
	}

	nextCursor := ""
	if hasMore && len(rows) > 0 {
		nextCursor = strconv.FormatInt(rows[len(rows)-1].FollowID, 10)
	}

	return &dto.FollowListResp{
		Users:      users,
		NextCursor: nextCursor,
		HasMore:    hasMore,
	}
}
