// Package handler — follow.go handles HTTP routing for the follow module
// (PRD-Phase2 §8 F-F01).
//
// Handlers are thin: parse request → call service → dispatch response. No
// business logic, no infrastructure access. All errors route through
// response.FromError, so *errcode.AppError values carry the correct HTTP
// status automatically.
//
// Route map (wired in cmd/server/main.go stage 7.8):
//
//	POST   /api/v1/users/:id/follow      Auth required   Follow
//	DELETE /api/v1/users/:id/follow      Auth required   Unfollow
//	GET    /api/v1/users/:id/followers   public          ListFollowers
//	GET    /api/v1/users/:id/following   public          ListFollowing
//
// Why follow/unfollow require Auth but the lists do not: following someone
// is an action attributed to the authenticated caller (PRD §8 AC-2 — an
// unauthenticated tap triggers a login prompt). Viewing a profile's
// follower/following list is a public read, consistent with
// GET /users/:id/posts being public.
//
// Added in Step 8 (follow module).
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

// FollowHandler holds the dependencies needed by all follow-related routes.
type FollowHandler struct {
	svc *service.FollowService
}

// NewFollowHandler constructs a FollowHandler.
func NewFollowHandler(svc *service.FollowService) *FollowHandler {
	return &FollowHandler{svc: svc}
}

// Follow handles POST /api/v1/users/:id/follow. Requires Auth middleware.
//
// :id is the user being followed; the follower is the authenticated caller.
func (h *FollowHandler) Follow(c *gin.Context) {
	followerID := middleware.GetUserID(c)
	if followerID == 0 {
		response.FromError(c, errcode.ErrUnauthorized)
		return
	}

	targetID, ok := parseUserIDParam(c)
	if !ok {
		return
	}

	resp, err := h.svc.Follow(c.Request.Context(), followerID, targetID)
	if err != nil {
		response.FromError(c, err)
		return
	}
	response.Success(c, resp)
}

// Unfollow handles DELETE /api/v1/users/:id/follow. Requires Auth middleware.
func (h *FollowHandler) Unfollow(c *gin.Context) {
	followerID := middleware.GetUserID(c)
	if followerID == 0 {
		response.FromError(c, errcode.ErrUnauthorized)
		return
	}

	targetID, ok := parseUserIDParam(c)
	if !ok {
		return
	}

	resp, err := h.svc.Unfollow(c.Request.Context(), followerID, targetID)
	if err != nil {
		response.FromError(c, err)
		return
	}
	response.Success(c, resp)
}

// ListFollowers handles GET /api/v1/users/:id/followers. Public endpoint.
//
// :id is the user whose followers ("粉丝") are listed.
func (h *FollowHandler) ListFollowers(c *gin.Context) {
	targetID, ok := parseUserIDParam(c)
	if !ok {
		return
	}

	var query dto.FollowListQuery
	if err := c.ShouldBindQuery(&query); err != nil {
		response.FromError(c, errcode.ErrInvalidParams)
		return
	}

	resp, err := h.svc.ListFollowers(c.Request.Context(), targetID, query.Cursor, query.Size)
	if err != nil {
		response.FromError(c, err)
		return
	}
	response.Success(c, resp)
}

// ListFollowing handles GET /api/v1/users/:id/following. Public endpoint.
//
// :id is the user whose followed accounts ("关注") are listed.
func (h *FollowHandler) ListFollowing(c *gin.Context) {
	targetID, ok := parseUserIDParam(c)
	if !ok {
		return
	}

	var query dto.FollowListQuery
	if err := c.ShouldBindQuery(&query); err != nil {
		response.FromError(c, errcode.ErrInvalidParams)
		return
	}

	resp, err := h.svc.ListFollowing(c.Request.Context(), targetID, query.Cursor, query.Size)
	if err != nil {
		response.FromError(c, err)
		return
	}
	response.Success(c, resp)
}

// parseUserIDParam extracts and validates the :id path parameter shared by
// all four follow routes. On failure it writes the error response itself and
// returns ok=false, so callers just `if !ok { return }` — the same
// write-and-signal pattern used across the handler package.
func parseUserIDParam(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.FromError(c, errcode.ErrInvalidParams)
		return 0, false
	}
	return id, true
}
