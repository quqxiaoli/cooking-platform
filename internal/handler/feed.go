// Package handler — feed.go is the HTTP binding for paginated post lists:
// the homepage / scene-filtered feed and the author timeline.
//
// Both endpoints share PostService.ListFeed / ListByUser semantics:
//   - Cursor-paginated, default size 20, max 50.
//   - Public (no Auth required).
//   - Only is_visible=1 rows are returned (MVP simplification).
//
// Why two separate handler methods rather than one polymorphic one:
//   - URL paths are different (/feed vs /users/:id/posts) so router
//     dispatch already separates them; combining at the handler layer
//     would re-introduce branching that the URL pattern just removed.
//   - The service-side filters are different (scene_tag filter vs author
//     filter), and a unified handler would gain a confusing "if/else by
//     param presence" branch.
//
// Future improvements:
//   - Reusable cursor/size param parser if more list endpoints land.
//   - Include `total` count when cheap to compute (MySQL COUNT can be
//     slow on millions of rows; offer it only when the table is small or
//     stay cursor-only forever — most modern feeds do).
//
// Added in Step 4 (content module).
package handler

import (
	"strconv"

	"cooking-platform/internal/middleware"
	"cooking-platform/internal/model/dto"
	"cooking-platform/internal/service"
	"cooking-platform/pkg/errcode"
	"cooking-platform/pkg/response"

	"github.com/gin-gonic/gin"
)

// FeedHandler hosts list-style read endpoints.
type FeedHandler struct {
	svc *service.PostService
}

// NewFeedHandler constructs a FeedHandler.
func NewFeedHandler(svc *service.PostService) *FeedHandler {
	return &FeedHandler{svc: svc}
}

// ListFeed handles GET /api/v1/feed[?scene_tag=N][&cursor=X][&size=N].
//
// Empty / missing query params all default to "first page, all scenes,
// size 20". Negative size and out-of-range scene_tag are rejected by gin
// binding; service then re-validates scene_tag against model.SceneTag
// for defence in depth.
func (h *FeedHandler) ListFeed(c *gin.Context) {
	var q dto.FeedQuery
	if err := c.ShouldBindQuery(&q); err != nil {
		response.BadRequest(c, errcode.ErrInvalidParams)
		return
	}

	viewerID := middleware.GetUserID(c) // 0 = anonymous (OptionalAuth)
	resp, err := h.svc.ListFeed(c.Request.Context(), viewerID, q)
	if err != nil {
		response.FromError(c, err)
		return
	}
	response.Success(c, resp)
}

// ListByUser handles GET /api/v1/users/:id/posts[?cursor=X][&size=N].
//
// :id is the author's user_id. Service returns is_visible=1 posts only —
// strangers don't see drafts/pending/rejected. The "author viewing own
// page" relaxation lands in Step 10 alongside the optional-auth middleware.
//
// We don't bind a struct here because there are only two query params
// and they have no validator tags worth wiring up; manual parsing is
// clearer than a 2-field struct + binding tag boilerplate.
func (h *FeedHandler) ListByUser(c *gin.Context) {
	authorID, err := parsePathID(c.Param("id"))
	if err != nil {
		response.BadRequest(c, errcode.ErrInvalidParams)
		return
	}

	cursor := c.Query("cursor")

	size, err := parseSizeQuery(c.Query("size"))
	if err != nil {
		response.BadRequest(c, errcode.ErrPageSizeInvalid)
		return
	}

	viewerID := middleware.GetUserID(c) // 0 = anonymous (OptionalAuth)
	resp, err := h.svc.ListByUser(c.Request.Context(), viewerID, authorID, cursor, size)
	if err != nil {
		response.FromError(c, err)
		return
	}
	response.Success(c, resp)
}

// parseSizeQuery parses an optional ?size=N query.
//
//	""           → 0 (service interprets as "use default 20")
//	"1".."50"    → that integer
//	other        → error
func parseSizeQuery(s string) (int, error) {
	if s == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, err
	}
	if n < 1 || n > 50 {
		return 0, strconv.ErrRange
	}
	return n, nil
}
