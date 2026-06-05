// Package service — post_service.go orchestrates the content pipeline:
// create posts, fetch detail, render feeds, and emit asynchronous events.
//
// [Step 9] Adds structured-step persistence and OSS URL whitelist
// enforcement. The constructor now takes a cfg.OSS by value, mirroring
// user_service.go.
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
//
// Added in Step 4 (content module). Refactored in Step 7 to share
// AuthorAssembler with the search module. Extended in Step 9 to support
// structured steps + OSS whitelist enforcement.
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
	"cooking-platform/pkg/config"
	"cooking-platform/pkg/errcode"
	"cooking-platform/pkg/oss"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

// PostService is the business orchestrator for the content module.
type PostService struct {
	postRepo    repository.PostRepository
	userRepo    repository.UserRepository
	feedCache   *cache.FeedCache
	writeMarker *cache.WriteMarker // [Fix #1] read-after-write force-master hint
	bus         event.EventPublisher
	assembler   *AuthorAssembler
	ossCfg      config.OSSConfig   // [Step 9] for image URL whitelist
	cacheCfg    config.CacheConfig // [Step 13] feed page TTL (was cache.FeedCacheTTL constant)
}

// NewPostService constructs a PostService.
//
// [Step 9] ossCfg drives the cover_url + step image_urls whitelist check.
// [Step 13] cacheCfg.FeedCacheTTL replaces the removed cache.FeedCacheTTL constant.
// [Fix #1] writeMarker is used to force master reads briefly after the author
// writes, so the author's own feed never appears to "lose" their new post due
// to slave replication lag.
func NewPostService(
	postRepo repository.PostRepository,
	userRepo repository.UserRepository,
	feedCache *cache.FeedCache,
	writeMarker *cache.WriteMarker,
	bus event.EventPublisher,
	assembler *AuthorAssembler,
	ossCfg config.OSSConfig,
	cacheCfg config.CacheConfig,
) *PostService {
	return &PostService{
		postRepo:    postRepo,
		userRepo:    userRepo,
		feedCache:   feedCache,
		writeMarker: writeMarker,
		bus:         bus,
		assembler:   assembler,
		ossCfg:      ossCfg,
		cacheCfg:    cacheCfg,
	}
}

// ── Create ──────────────────────────────────────────────────────────────────

// Create persists a new post (and its steps, if any) and triggers the
// asynchronous side effects.
//
// [Step 9] Two code paths now coexist:
//
//   - Legacy text-only (req.Steps empty): single-row INSERT via Create.
//     Existing clients continue to work unchanged.
//
//   - Structured (req.Steps non-empty): transaction-wrapped INSERT post +
//     INSERT post_steps via CreateWithSteps. Atomic — readers never see
//     a half-published post (post exists, no steps).
//
// Cover URL and each step's image URLs must pass the OSS whitelist —
// defence in depth on top of the presign+callback flow.
func (s *PostService) Create(ctx context.Context, userID int64, req dto.CreatePostReq) (*dto.CreatePostResp, error) {
	// Defensive scene-tag validation (DTO binding already covers 1..8).
	sceneTag := model.SceneTag(req.SceneTag)
	if !sceneTag.IsValid() {
		return nil, errcode.ErrSceneTagInvalid
	}

	title := strings.TrimSpace(req.Title)
	if title == "" {
		return nil, errcode.ErrTitleEmpty
	}

	// [Step 9] Cover URL whitelist (empty allowed = no cover).
	if !oss.IsAllowedURL(req.CoverURL, s.ossCfg.URLPrefix) {
		return nil, errcode.ErrUploadURLNotAllowed
	}

	// [Step 9] Step validation. DTO binding caps len ≤ 30 and each text
	// ≤ 500; we re-check the cap as defence (someone may swap validator
	// libraries) and enforce image URL whitelist.
	if len(req.Steps) > 30 {
		return nil, errcode.ErrPostStepsInvalid
	}
	for _, st := range req.Steps {
		if !oss.AllAllowed(st.ImageURLs, s.ossCfg.URLPrefix) {
			return nil, errcode.ErrUploadURLNotAllowed
		}
	}

	// [Step 10] New posts start invisible (is_visible=0, audit_status=pending).
	// AuditConsumer receives the AuditEvent published below, calls the content
	// safety API, and flips is_visible to 1 on approval. MockAuditor (dev mode)
	// resolves in milliseconds so posts become visible almost immediately.
	now := time.Now()
	p := &model.Post{
		UserID:      userID,
		Title:       title,
		Content:     req.Content,
		SceneTag:    sceneTag,
		CoverURL:    req.CoverURL,
		IsVisible:   model.PostInvisible,
		AuditStatus: model.AuditStatusPending,
		CreatedAt:   now,
		UpdatedAt:   now,
	}

	if len(req.Steps) == 0 {
		// Legacy text-only path.
		if err := s.postRepo.Create(ctx, p); err != nil {
			return nil, fmt.Errorf("create post: %w", err)
		}
	} else {
		// Structured-steps path — atomic transaction.
		steps := make([]*model.PostStep, 0, len(req.Steps))
		for i, st := range req.Steps {
			steps = append(steps, &model.PostStep{
				StepNo:    uint8(i + 1), // 1-indexed; uk_post_step is (post_id, step_no)
				Text:      st.Text,
				ImageURLs: model.StringArray(st.ImageURLs),
				CreatedAt: now,
			})
		}
		if err := s.postRepo.CreateWithSteps(ctx, p, steps); err != nil {
			return nil, fmt.Errorf("create post with steps: %w", err)
		}
	}

	// [Fix #1] Stamp the author's write marker so any read in the next ~5s
	// (typically the client refreshing its own author page) is routed to the
	// master and never sees a slave-lag "post vanished" gap.
	s.writeMarker.Mark(ctx, userID)

	// Async side effects — best-effort, never fail the user-visible op.
	// publishPostEvent → CountConsumer increments users.post_count.
	// publishAuditEvent → AuditConsumer calls content safety API, then
	//   flips is_visible + audit_status once the verdict is ready.
	s.publishPostEvent(ctx, p)
	s.publishAuditEvent(ctx, p)
	s.bumpFeedVersion(ctx, p.ID)

	return &dto.CreatePostResp{
		PostID:      p.ID,
		AuditStatus: p.AuditStatus,
		IsVisible:   p.IsVisible,
		CreatedAt:   p.CreatedAt.UnixMilli(),
	}, nil
}

// ── GetDetail ───────────────────────────────────────────────────────────────

// GetDetail loads a post by ID, attaches the author snapshot + steps, and
// triggers PV deduplication + event emission.
//
// [Step 9] Step loading is best-effort: when post_steps query fails, we
// log + return an empty Steps slice rather than 500. Frontend will fall
// back to rendering Content (legacy text-only behaviour) — the page is
// still useful.
func (s *PostService) GetDetail(ctx context.Context, postID, viewerID int64, viewerIP string) (*dto.PostDetailResp, error) {
	p, err := s.postRepo.FindByID(ctx, postID)
	if err != nil {
		if errors.Is(err, repository.ErrPostNotFound) {
			return nil, errcode.ErrPostNotFound
		}
		return nil, fmt.Errorf("find post: %w", err)
	}

	// MVP visibility rule: anyone (author included) only sees visible posts.
	if p.IsVisible != model.PostVisible {
		return nil, errcode.ErrPostNotFound
	}

	author, err := s.assembler.LoadOne(ctx, p.UserID)
	if err != nil {
		return nil, err
	}

	// [Step 9] Load structured steps. Empty slice = legacy post.
	steps, err := s.postRepo.LoadSteps(ctx, postID)
	if err != nil {
		zap.L().Warn("load post steps failed; degrading to text-only render",
			zap.Int64("post_id", postID),
			zap.Error(err),
		)
		steps = nil
	}

	// Best-effort PV recording.
	s.recordPV(ctx, postID, viewerID, viewerIP)

	return s.toDetailResp(p, author, steps), nil
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

	scene := q.SceneTag

	ver, verErr := s.feedCache.GetFeedVersion(ctx)
	if verErr != nil {
		zap.L().Warn("get feed version failed; bypassing cache", zap.Error(verErr))
	} else {
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

	if verErr == nil {
		if data, jerr := json.Marshal(resp); jerr == nil {
			if cerr := s.feedCache.SetFeed(ctx, scene, ver, cursorTime, data, s.cacheCfg.FeedCacheTTL); cerr != nil {
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
func (s *PostService) ListByUser(ctx context.Context, authorID int64, cursor string, size int) (*dto.FeedResp, error) {
	size = normaliseSize(size)

	cursorTime, err := parseCursor(cursor)
	if err != nil {
		return nil, errcode.ErrCursorInvalid
	}

	// [Fix #1] If authorID has a fresh write marker, force this read to the
	// master. The check is a single Redis EXISTS — cheap enough to do per
	// request, and the marker auto-expires in 5s so we don't permanently
	// degrade read-write splitting.
	if s.writeMarker.Has(ctx, authorID) {
		ctx = repository.WithForceMaster(ctx)
	}

	posts, err := s.postRepo.ListByUser(ctx, authorID, false, cursorTime, size)
	if err != nil {
		return nil, fmt.Errorf("list by user: %w", err)
	}

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

// toDetailResp builds the post-detail wire DTO.
//
// [Step 9] Steps is converted from []*model.PostStep to []dto.PostStepResp.
// We construct the slice with make(..., 0, len(steps)) so an empty steps
// list serialises as a JSON empty array (`[]`) rather than `null` — keeps
// frontend `.steps.map(...)` calls safe without nil-guards.
func (s *PostService) toDetailResp(p *model.Post, author *model.User, steps []*model.PostStep) *dto.PostDetailResp {
	stepDTOs := make([]dto.PostStepResp, 0, len(steps))
	for _, st := range steps {
		stepDTOs = append(stepDTOs, dto.PostStepResp{
			StepNo:    st.StepNo,
			Text:      st.Text,
			ImageURLs: []string(st.ImageURLs),
		})
	}

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
		Steps:       stepDTOs,
		AuditStatus: p.AuditStatus,
		IsVisible:   p.IsVisible,
		CreatedAt:   p.CreatedAt.UnixMilli(),
		UpdatedAt:   p.UpdatedAt.UnixMilli(),
	}
}

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

func nextCursorOf(posts []*model.Post, size int) string {
	if len(posts) < size {
		return ""
	}
	last := posts[len(posts)-1]
	return strconv.FormatInt(last.CreatedAt.UnixMilli(), 10)
}

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

// publishAuditEvent sends a pending-audit notification to TopicAudit.
// AuditStatus=0 (AuditStatusPending) signals AuditConsumer that this is a
// submission request, not a result. The consumer will call the content safety
// API, obtain the real verdict, and update audit_status + is_visible in DB.
//
// RawResponse and Remark are empty in the submission event — the consumer
// fills those after calling the API and writes them to audit_log.
func (s *PostService) publishAuditEvent(ctx context.Context, p *model.Post) {
	now := time.Now().UnixMilli()
	payload := event.AuditEvent{
		EventID:     uuid.NewString(),
		PostID:      p.ID,
		AuthorID:    p.UserID,
		AuditStatus: int8(model.AuditStatusPending),
		Remark:      "",
		RawResponse: "",
		Timestamp:   now,
	}
	evt := event.Event{
		ID:        payload.EventID,
		Topic:     event.TopicAudit,
		Timestamp: now,
		Payload:   event.MustMarshalPayload(payload),
	}
	if err := s.bus.Publish(ctx, evt); err != nil {
		zap.L().Warn("publish audit event failed",
			zap.Int64("post_id", p.ID),
			zap.Error(err),
		)
	}
}

func maskIP(ip string) string {
	parts := strings.Split(ip, ".")
	if len(parts) != 4 {
		return ""
	}
	return parts[0] + "." + parts[1] + ".*.*"
}
