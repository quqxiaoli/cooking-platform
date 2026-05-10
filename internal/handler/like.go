// Package handler — like.go exposes the like-module HTTP endpoints.
//
// Routes (wired in cmd/server/main.go's setupRouter):
//
//	POST   /api/v1/posts/:id/like   → Like        (auth + rate-limit)
//	DELETE /api/v1/posts/:id/like   → Unlike      (auth)
//	GET    /api/v1/posts/:id/like   → LikeStatus  (auth)
//
// ── Why DELETE /like instead of POST /unlike ────────────────────────────────
//
// The "like" relationship between user and post is a *resource*: it either
// exists (the user has liked the post) or doesn't. POST creates it,
// DELETE removes it, GET inspects it. This is canonical REST — no
// imperative-verb URLs (`/unlike`), no "action" suffixes — and it pairs
// naturally with the idempotent semantics our service layer guarantees:
// re-POSTing a like or re-DELETEing one are both safe no-ops.
//
// ── Why GET /like requires Auth ─────────────────────────────────────────────
//
// The endpoint answers "have *I* liked this post?" — there's no meaningful
// answer for an anonymous viewer. We could let unauthenticated GETs return
// {liked: false, count: N} as a courtesy, but that splits the response
// shape's meaning across two visitor classes and complicates front-end
// handling. Cleaner: require auth, let public detail fetches use the
// post's `like_count` field (Step 4 already returns it on /posts/:id).
//
// ── Why no JSON body parsing here ───────────────────────────────────────────
//
// All inputs come from URL/JWT: post_id from :id, user_id from JWT.
// No body to bind. Handler stays trivial — a 10-line wrapper around
// service calls. This is the "thin handler" philosophy carried over
// from Step 3 user_handler.go and Step 4 post_handler.go.
//
// Added in Step 5 (like module).
package handler

import (
	"errors"
	"net/http"
	"strconv"

	"cooking-platform/internal/middleware"
	"cooking-platform/internal/service"
	"cooking-platform/pkg/errcode"
	"cooking-platform/pkg/response"

	"github.com/gin-gonic/gin"
)

// LikeHandler is the HTTP entry point for the like module.
type LikeHandler struct {
	svc *service.LikeService
}

// NewLikeHandler constructs a LikeHandler.
func NewLikeHandler(svc *service.LikeService) *LikeHandler {
	return &LikeHandler{svc: svc}
}

// Like handles POST /api/v1/posts/:id/like.
//
// Auth middleware ensures user_id is present in the gin context. The post
// id comes from the URL path; non-numeric or non-positive values are
// rejected with 400001 (ErrInvalidParams) before any service call —
// the underlying ErrPostNotFound (412104) is reserved for "valid id,
// no such post".
func (h *LikeHandler) Like(c *gin.Context) {
	postID, ok := parsePostID(c)
	if !ok {
		response.FromError(c, errcode.ErrInvalidParams)
		return
	}
	userID := middleware.GetUserID(c)
	// Auth middleware is upstream so userID should be non-zero. Defensive
	// check in case route is misconfigured (better than passing 0 down).
	if userID == 0 {
		response.FromError(c, errcode.ErrUnauthorized)
		return
	}

	resp, err := h.svc.Like(c.Request.Context(), userID, postID)
	if err != nil {
		mapServiceErr(c, err)
		return
	}
	response.Success(c, resp)
}

// Unlike handles DELETE /api/v1/posts/:id/like.
func (h *LikeHandler) Unlike(c *gin.Context) {
	postID, ok := parsePostID(c)
	if !ok {
		response.FromError(c, errcode.ErrInvalidParams)
		return
	}
	userID := middleware.GetUserID(c)
	if userID == 0 {
		response.FromError(c, errcode.ErrUnauthorized)
		return
	}

	resp, err := h.svc.Unlike(c.Request.Context(), userID, postID)
	if err != nil {
		mapServiceErr(c, err)
		return
	}
	response.Success(c, resp)
}

// GetLikeStatus handles GET /api/v1/posts/:id/like.
func (h *LikeHandler) GetLikeStatus(c *gin.Context) {
	postID, ok := parsePostID(c)
	if !ok {
		response.FromError(c, errcode.ErrInvalidParams)
		return
	}
	userID := middleware.GetUserID(c)
	if userID == 0 {
		response.FromError(c, errcode.ErrUnauthorized)
		return
	}

	resp, err := h.svc.GetLikeStatus(c.Request.Context(), userID, postID)
	if err != nil {
		mapServiceErr(c, err)
		return
	}
	response.Success(c, resp)
}

// parsePostID extracts and validates the :id URL param.
//
// Returns (postID, true) on success, (0, false) on any parse failure
// (non-numeric, ≤ 0, overflow). The caller maps false to 400001.
func parsePostID(c *gin.Context) (int64, bool) {
	raw := c.Param("id")
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

// mapServiceErr converts a service-layer error to the appropriate HTTP
// response. AppErrors (errcode.*) carry their own HTTP status; opaque
// errors map to 500.
//
// Co-located here rather than in the response package because the mapping
// is module-shaped: future modules may want different "service error
// → http" rules (e.g. wrapping infrastructure errors with extra context
// for ops dashboards).
func mapServiceErr(c *gin.Context, err error) {
	var appErr *errcode.AppError
	if errors.As(err, &appErr) {
		response.FromError(c, appErr)
		return
	}
	// Unexpected error — server-side fault. Don't leak internals to the
	// client; the request_id in the response gives ops a correlation key.
	c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{
		"code":       errcode.ErrServer.Code,
		"msg":        errcode.ErrServer.Msg,
		"request_id": c.GetString("X-Request-ID"),
	})
}
