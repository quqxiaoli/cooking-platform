// Package service — post_service.go orchestrates the content pipeline:
// create posts, fetch detail, render feeds, and emit asynchronous events.
//
// Service rules in this codebase (consistent with user_service.go):
//
//  1. Handlers stay thin: parse → call service → respond. No business
//     decisions in handlers, no SQL in handlers.
//  2. Service holds the business invariants (visibility checks, author
//     snapshot embedding, cache version coordination, event publishing).
//  3. Repository methods do exactly what their name says — no caching,
//     no event publishing inside repository.
//  4. Cache reads are best-effort: on Redis error or unmarshal failure
//     we silently fall through to the source of truth (MySQL).
//  5. Event publishing failures NEVER abort the user-visible operation.
//     A failed PostEvent means CountConsumer (Step 5+) misses one delta;
//     acceptable. A failed BumpFeedVersion means stale feeds for up to
//     300 seconds; acceptable.
//
// ── Author-snapshot assembly: shared with search since Step 7 ──────────────
//
// The "load authors, embed author brief, build PostListItem" logic used to
// live as private methods here (loadAuthor / toListItem / makeAuthorBrief /
// uniqueAuthorIDs). Step 7 extracted it into AuthorAssembler so the search
// module reuses the exact same implementation. PostService now holds an
// *AuthorAssembler and delegates to it. Only toDetailResp stays local — the
// post-detail shape is not shared with search.
//
// ── MVP simplifications & the deferred work they imply ─────────────────────
//
//   - is_visible is set to 1 at creation and audit_status to 0 (pending).
//     Step 10 will flip this: new posts go is_visible=0 and AuditConsumer
//     promotes them after machine review. The feed query is identical
//     either way (`WHERE is_visible=1`), so the change is invisible to
//     reader code.
//
//   - The "author of an invisible post viewing their own page sees it"
//     rule is NOT implemented in MVP. Both detail and author-page deny
//     access uniformly when is_visible=0. Reason: the routes don't carry
//     Auth middleware (they're public for performance), so we don't
//     reliably know if the viewer IS the author. Step 10 will introduce
//     an "optional auth" middleware that parses the bearer token if
//     present without rejecting on absence; that's the right place to
//     add the author-self-view branch.
//
//   - view_count stays at 0 in MVP because PVConsumer ships in Step 5.
//     The PVEvent path is already wired here so Step 5 just plugs in
//     the consumer without any service-layer change.
//
//   - Feed author loading is N+1 (one FindByID per unique author).
//     Acceptable for size<=50 and ~1ms per query. Add userRepo.FindByIDs
//     when a "follow feed" view appears that pulls 100+ posts at once.
//
// Future improvements:
//   - Wrap Create + BumpFeedVersion in a single MQ-driven flow (Step 13)
//     so the bump truly fires only after MySQL durably persisted the row.
//     Today's order (Create → Bump → Publish) is fine for Channel mode.
//   - Strict cursor pagination (LIMIT size+1) to eliminate the last-page
//     false-positive has_more. Costs an extra row read; not worth it
//     until users complain about the brief empty "second tap" at the end.
//   - Cache the assembled FeedResp (with embedded authors) rather than
//     re-querying users on every miss. Today the cache stores the JSON
//     of the full response, so re-querying happens only on miss anyway.
//
// Added in Step 4 (content module). Refactored in Step 7 to share
// AuthorAssembler with the search module.
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"cooking-platform/internal/cache"
	"cooking-platform/internal/event"
	"cooking-platform/internal/model"
	"cooking-platform/internal/model/dto"
	"cooking-platform/internal/repository"
	"cooking-platform/pkg/errcode"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

// PostService is the business orchestrator for the content module.
type PostService struct {
	postRepo  repository.PostRepository
	userRepo  repository.UserRepository
	feedCache *cache.FeedCache
	bus       event.EventPublisher
	assembler *AuthorAssembler
}

// NewPostService constructs a PostService.
//
// Dependencies are injected (not constructed inside) so tests can swap in
// fakes and so the same instance is shared across goroutines without a
// per-request constructor cost. Standard wiring pattern in this codebase.
//
// Step 7: takes an *AuthorAssembler so feed/author-page list assembly shares
// one implementation with the search module. userRepo is still held for
// loadAuthor-free direct needs and for symmetry with the original wiring.
func NewPostService(
	postRepo repository.PostRepository,
	userRepo repository.UserRepository,
	feedCache *cache.FeedCache,
	bus event.EventPublisher,
	assembler *AuthorAssembler,
) *PostService {
	return &PostService{
		postRepo:  postRepo,
		userRepo:  userRepo,
		feedCache: feedCache,
		bus:       bus,
		assembler: assembler,
	}
}

// ── Create ──────────────────────────────────────────────────────────────────

// Create persists a new post and triggers the asynchronous side effects
// (PostEvent for downstream counters, feed version bump for cache invalidation).
//
// Order is intentional:
//  1. Validate (cheap, fail-fast)
//  2. INSERT MySQL (the only thing the user truly cares about; if it
//     fails we return an error and nothing else has happened yet)
//  3. Publish PostEvent (best-effort; failure logged not propagated)
//  4. INCR feed:ver (best-effort; same logic)
func (s *PostService) Create(ctx context.Context, userID int64, req dto.CreatePostReq) (*dto.CreatePostResp, error) {
	// Defensive scene-tag validation: gin's binding layer already rejects
	// 0/9+ but we re-check here in case someone bypasses binding (e.g.
	// internal RPC, future direct service call, validator library swap).
	sceneTag := model.SceneTag(req.SceneTag)
	if !sceneTag.IsValid() {
		return nil, errcode.ErrSceneTagInvalid
	}

	// Trim title: pure-whitespace titles like "   " pass min=1 binding
	// but are obviously empty. Trim once here so the rest of the system
	// sees a canonical value (matters for FULLTEXT indexing too).
	title := strings.TrimSpace(req.Title)
	if title == "" {
		return nil, errcode.ErrTitleEmpty
	}

	// MVP: directly visible, audit_status=pending (=0). Step 10 changes
	// IsVisible default to PostInvisible at this site.
	//
	// Why we set CreatedAt/UpdatedAt explicitly rather than rely on GORM's
	// autoCreateTime: when a model field carries `default:CURRENT_TIMESTAMP(3)`
	// tag, GORM v2 marks the field as "DB-generated" and skips both Go-side
	// fillup and the post-insert read-back. The DB stores the right value,
	// but `p.CreatedAt` returned to the caller stays as time.Time zero
	// (-62135596800000 in UnixMilli). Setting time.Now() here makes the
	// returned response reliable without paying for a SELECT after INSERT.
	now := time.Now()
	p := &model.Post{
		UserID:      userID,
		Title:       title,
		Content:     req.Content,
		SceneTag:    sceneTag,
		CoverURL:    req.CoverURL,
		IsVisible:   model.PostVisible,
		AuditStatus: model.AuditStatusPending,
		CreatedAt:   now,
		UpdatedAt:   now,
	}

	if err := s.postRepo.Create(ctx, p); err != nil {
		return nil, fmt.Errorf("create post: %w", err)
	}

	// Async side effects — best-effort, never fail the user-visible op.
	s.publishPostEvent(ctx, p)
	s.bumpFeedVersion(ctx, p.ID)

	return &dto.CreatePostResp{
		PostID:      p.ID,
		AuditStatus: p.AuditStatus,
		IsVisible:   p.IsVisible,
		CreatedAt:   p.CreatedAt.UnixMilli(),
	}, nil
}

// ── GetDetail ───────────────────────────────────────────────────────────────

// GetDetail loads a post by ID, attaches the author snapshot, and triggers
// PV deduplication + event emission.
//
// viewerID == 0 means the request is unauthenticated (public detail route).
// viewerIP is used only for PV dedup when viewerID == 0; we mask it before
// it ever reaches Redis or downstream events.
func (s *PostService) GetDetail(ctx context.Context, postID, viewerID int64, viewerIP string) (*dto.PostDetailResp, error) {
	p, err := s.postRepo.FindByID(ctx, postID)
	if err != nil {
		if errors.Is(err, repository.ErrPostNotFound) {
			return nil, errcode.ErrPostNotFound
		}
		return nil, fmt.Errorf("find post: %w", err)
	}

	// MVP visibility rule: anyone (author included) only sees visible posts.
	// See file header for why and how Step 10 will relax this.
	if p.IsVisible != model.PostVisible {
		return nil, errcode.ErrPostNotFound
	}

	author, err := s.assembler.LoadOne(ctx, p.UserID)
	if err != nil {
		return nil, err
	}

	// Best-effort PV recording. Failure to record does not block the read.
	s.recordPV(ctx, postID, viewerID, viewerIP)

	return s.toDetailResp(p, author), nil
}

// ── ListFeed ────────────────────────────────────────────────────────────────

// ListFeed returns one page of the home feed (or scene-filtered feed),
// using the version-keyed cache before falling back to MySQL.
func (s *PostService) ListFeed(ctx context.Context, q dto.FeedQuery) (*dto.FeedResp, error) {
	size := normaliseSize(q.Size)

	cursorTime, err := parseCursor(q.Cursor)
	if err != nil {
		return nil, errcode.ErrCursorInvalid
	}

	// Scene-tag was already 1..8-validated by gin binding; 0 is the
	// "all scenes" sentinel for the cache key path.
	scene := q.SceneTag

	ver, verErr := s.feedCache.GetFeedVersion(ctx)
	if verErr != nil {
		zap.L().Warn("get feed version failed; bypassing cache", zap.Error(verErr))
	} else {
		// Cache lookup. Any error or unmarshal failure → silent fallthrough.
		if cached, cerr := s.feedCache.GetFeed(ctx, scene, ver, cursorTime); cerr != nil {
			zap.L().Warn("get feed cache failed; bypassing cache",
				zap.Int8("scene", scene),
				zap.Int64("ver", ver),
				zap.Error(cerr),
			)
		} else if cached != nil {
			var resp dto.FeedResp
			if uerr := json.Unmarshal(cached, &resp); uerr == nil {
				return &resp, nil
			}
			// Corrupt cache entry — log and refetch.
			zap.L().Warn("feed cache unmarshal failed; refetching from db",
				zap.Int8("scene", scene),
				zap.Int64("ver", ver),
			)
		}
	}

	posts, err := s.postRepo.ListFeed(ctx, scene, cursorTime, size)
	if err != nil {
		return nil, fmt.Errorf("list feed: %w", err)
	}

	resp, err := s.assembleFeed(ctx, posts, size)
	if err != nil {
		return nil, err
	}

	// Write-back to cache. We cache empty pages too: an empty homepage
	// gets a 5-minute window of zero DB load until the next post creation
	// bumps the version.
	if verErr == nil {
		if data, jerr := json.Marshal(resp); jerr == nil {
			if cerr := s.feedCache.SetFeed(ctx, scene, ver, cursorTime, data, cache.FeedCacheTTL); cerr != nil {
				zap.L().Warn("set feed cache failed",
					zap.Int8("scene", scene),
					zap.Int64("ver", ver),
					zap.Error(cerr),
				)
			}
		}
	}

	return resp, nil
}

// ── ListByUser ──────────────────────────────────────────────────────────────

// ListByUser returns one page of an author's public posts.
//
// MVP simplification: includeInvisible is hardcoded false. See file
// header for why and what Step 10 will change.
//
// Not cached: per-author timelines have low read fan-out (each user has
// ~1 visitor at a time). Caching them means more cache keys without a
// proportional hit-rate gain. Revisit when a creator's profile becomes a
// hot spot (e.g. >100 RPS on a single :id).
func (s *PostService) ListByUser(ctx context.Context, authorID int64, cursor string, size int) (*dto.FeedResp, error) {
	size = normaliseSize(size)

	cursorTime, err := parseCursor(cursor)
	if err != nil {
		return nil, errcode.ErrCursorInvalid
	}

	posts, err := s.postRepo.ListByUser(ctx, authorID, false, cursorTime, size)
	if err != nil {
		return nil, fmt.Errorf("list by user: %w", err)
	}

	// One author for the entire page → one user lookup, not N.
	var author *model.User
	if len(posts) > 0 {
		a, lerr := s.assembler.LoadOne(ctx, authorID)
		if lerr != nil {
			return nil, lerr
		}
		author = a
	}

	items := make([]dto.PostListItem, 0, len(posts))
	for _, p := range posts {
		items = append(items, BuildListItem(p, author))
	}

	return &dto.FeedResp{
		Posts:      items,
		NextCursor: nextCursorOf(posts, size),
		HasMore:    len(posts) >= size,
	}, nil
}

// ── Private helpers ─────────────────────────────────────────────────────────

// publishPostEvent fires a PostEvent for downstream consumers (Step 5+
// CountConsumer for users.post_count, future feed-rebuild consumers).
// Channel-mode failures are logged but never propagated.
func (s *PostService) publishPostEvent(ctx context.Context, p *model.Post) {
	now := time.Now().UnixMilli()
	payload := event.PostEvent{
		EventID:   uuid.NewString(),
		PostID:    p.ID,
		AuthorID:  p.UserID,
		SceneTag:  int8(p.SceneTag),
		Timestamp: now,
	}
	evt := event.Event{
		ID:        payload.EventID,
		Topic:     event.TopicPost,
		Timestamp: now,
		Payload:   event.MustMarshalPayload(payload),
	}
	if err := s.bus.Publish(ctx, evt); err != nil {
		zap.L().Warn("publish post event failed",
			zap.Int64("post_id", p.ID),
			zap.Error(err),
		)
	}
}

// bumpFeedVersion invalidates ALL feed cache entries by stepping the
// global version counter. The cost is constant (one INCR), regardless of
// how many cache entries exist. See feed_cache.go header for full
// rationale.
func (s *PostService) bumpFeedVersion(ctx context.Context, postID int64) {
	newVer, err := s.feedCache.BumpFeedVersion(ctx)
	if err != nil {
		zap.L().Warn("bump feed version failed",
			zap.Int64("post_id", postID),
			zap.Error(err),
		)
		return
	}
	zap.L().Debug("feed version bumped",
		zap.Int64("post_id", postID),
		zap.Int64("new_version", newVer),
	)
}

// recordPV deduplicates and (on first view) emits a PVEvent.
//
// MVP: PVConsumer ships in Step 5. Until then PVEvents are published but
// dropped (no subscriber on TopicPV → ChannelBus.Publish is a no-op).
// view_count stays at 0 across MVP — known limitation, registered as
// PRD deviation for Step 4.
func (s *PostService) recordPV(ctx context.Context, postID, viewerID int64, viewerIP string) {
	firstView, err := s.feedCache.MarkPVSeen(ctx, postID, viewerID, viewerIP)
	if err != nil {
		zap.L().Warn("mark pv seen failed",
			zap.Int64("post_id", postID),
			zap.Error(err),
		)
		return
	}
	if !firstView {
		return
	}

	now := time.Now().UnixMilli()
	payload := event.PVEvent{
		EventID:   uuid.NewString(),
		PostID:    postID,
		ViewerID:  viewerID,
		IP:        maskIP(viewerIP),
		Timestamp: now,
	}
	evt := event.Event{
		ID:        payload.EventID,
		Topic:     event.TopicPV,
		Timestamp: now,
		Payload:   event.MustMarshalPayload(payload),
	}
	if err := s.bus.Publish(ctx, evt); err != nil {
		zap.L().Warn("publish pv event failed",
			zap.Int64("post_id", postID),
			zap.Error(err),
		)
	}
}

// assembleFeed converts a []*model.Post into a FeedResp with author
// snapshots embedded. Delegates author loading + list-item building to the
// shared AuthorAssembler (Step 7). Performs N+1 author loads — see file
// header for the deferred batch-load improvement.
func (s *PostService) assembleFeed(ctx context.Context, posts []*model.Post, size int) (*dto.FeedResp, error) {
	authorMap, err := s.assembler.LoadMap(ctx, posts)
	if err != nil {
		return nil, err
	}

	return &dto.FeedResp{
		Posts:      BuildListItems(posts, authorMap),
		NextCursor: nextCursorOf(posts, size),
		HasMore:    len(posts) >= size,
	}, nil
}

// toDetailResp builds the post-detail wire DTO. Kept local to PostService
// (not moved to AuthorAssembler) because the detail shape — Content,
// AuditStatus, UpdatedAt — is specific to GET /posts/:id and not shared
// with the search or feed list paths.
func (s *PostService) toDetailResp(p *model.Post, author *model.User) *dto.PostDetailResp {
	return &dto.PostDetailResp{
		ID:          p.ID,
		Title:       p.Title,
		SceneTag:    int8(p.SceneTag),
		SceneName:   p.SceneTag.Name(),
		Content:     p.Content,
		CoverURL:    p.CoverURL,
		LikeCount:   p.LikeCount,
		ViewCount:   p.ViewCount,
		Author:      BuildAuthorBrief(p.UserID, author),
		AuditStatus: p.AuditStatus,
		IsVisible:   p.IsVisible,
		CreatedAt:   p.CreatedAt.UnixMilli(),
		UpdatedAt:   p.UpdatedAt.UnixMilli(),
	}
}

// parseCursor converts the wire-string cursor into a time.Time.
//
//	""      → time.Time{} (sentinel for "first page")
//	"<ms>"  → time.UnixMilli(ms)
//	other   → ErrCursorInvalid
//
// Negative or zero milliseconds are also rejected — they would map to
// 1970 and confuse "is this the first page?" checks downstream.
func parseCursor(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	ms, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return time.Time{}, err
	}
	if ms <= 0 {
		return time.Time{}, errors.New("cursor must be a positive unix-milli")
	}
	return time.UnixMilli(ms), nil
}

// nextCursorOf returns the encoded cursor string for the next page, or ""
// if this page is the last one.
//
// Last-page detection is "page came back smaller than asked-for size". A
// page exactly at `size` could be the very last page (false positive on
// has_more); the next call returns 0 results. Acceptable false positive
// in exchange for not reading size+1 rows every time. See file header.
func nextCursorOf(posts []*model.Post, size int) string {
	if len(posts) < size {
		return ""
	}
	last := posts[len(posts)-1]
	return strconv.FormatInt(last.CreatedAt.UnixMilli(), 10)
}

// normaliseSize clamps page size to [1, 50] with default 20. Shared with
// SearchService (search_service.go) — both modules use the same paging policy.
func normaliseSize(size int) int {
	switch {
	case size <= 0:
		return 20
	case size > 50:
		return 50
	default:
		return size
	}
}

// maskIP redacts the last two octets of an IPv4 address, or returns "" for
// non-IPv4 (IPv6, malformed). The mask preserves geo-coarse information
// for analytics without storing precise client identifiers in events.
//
// IPv6 deserves real masking later (preserve /64 prefix). Today's mobile
// audience is mostly IPv4, so we keep the simple branch and TODO IPv6
// when we see meaningful v6 traffic.
func maskIP(ip string) string {
	parts := strings.Split(ip, ".")
	if len(parts) != 4 {
		return ""
	}
	return parts[0] + "." + parts[1] + ".*.*"
}
