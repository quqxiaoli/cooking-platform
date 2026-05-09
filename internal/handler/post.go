// Package handler — post.go is the HTTP-binding for write/read operations
// on a single post: create and detail.
//
// Handlers in this codebase follow the "thin handler" pattern:
//  1. Parse and validate the wire input
//  2. Pull authenticated identity (when applicable) from middleware context
//  3. Call exactly one service method
//  4. Translate the service's response into HTTP via response.* helpers
//
// Anything more (DB queries, business decisions, multi-step orchestration)
// belongs in the service layer. The boundary makes route handlers boringly
// short and individually unit-testable; service unit tests then cover
// behaviour without requiring an HTTP harness.
//
// Error mapping:
//   - service-returned *errcode.AppError      → response.FromError dispatches to the right HTTP status
//   - bind/validation failures                → response.BadRequest with errcode.ErrInvalidParams
//   - missing auth identity (Auth not chained) → response.Unauthorized (defensive; should not happen)
//
// Future improvements:
//   - Introduce an "optional auth" middleware so GetDetail can let an
//     authenticated author see their own pending posts (Step 10 task).
//   - Per-route metrics (handler_name + status_code) once Step 16 lands.
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

// PostHandler hosts handlers for /api/v1/posts/*.
type PostHandler struct {
	svc *service.PostService
}

// NewPostHandler constructs a PostHandler.
func NewPostHandler(svc *service.PostService) *PostHandler {
	return &PostHandler{svc: svc}
}

// Create handles POST /api/v1/posts.
//
// Route is wrapped by middleware.Auth + middleware.RateLimit (limit:pub,
// 20 per 24h, see PRD-Phase3 §6.3). Auth guarantees a non-zero user_id
// in the gin context; we still defensive-check to keep behaviour sane if
// the route is ever mis-mounted without Auth in front.
func (h *PostHandler) Create(c *gin.Context) {
	var req dto.CreatePostReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, errcode.ErrInvalidParams)
		return
	}

	uid := middleware.GetUserID(c)
	if uid == 0 {
		// Auth middleware should have rejected this request already.
		// Reaching here means a router misconfiguration; log loudly via
		// 401 so the operator notices in the access log.
		response.Unauthorized(c, errcode.ErrUnauthorized)
		return
	}

	resp, err := h.svc.Create(c.Request.Context(), uid, req)
	if err != nil {
		response.FromError(c, err)
		return
	}
	response.Success(c, resp)
}

// GetDetail handles GET /api/v1/posts/:id.
//
// Public route — no Auth middleware. We still call middleware.GetUserID
// in case an upstream feature mounts an "optional auth" middleware later;
// today it always returns 0 here.
//
// viewerIP comes from gin's ClientIP(), which honours X-Forwarded-For
// when configured (gin.SetTrustedProxies must be set in production for
// correct values; that's a Step 18 deployment concern).
func (h *PostHandler) GetDetail(c *gin.Context) {
	postID, err := parsePathID(c.Param("id"))
	if err != nil {
		response.BadRequest(c, errcode.ErrInvalidParams)
		return
	}

	viewerID := middleware.GetUserID(c) // 0 = anonymous
	viewerIP := c.ClientIP()

	resp, err := h.svc.GetDetail(c.Request.Context(), postID, viewerID, viewerIP)
	if err != nil {
		response.FromError(c, err)
		return
	}
	response.Success(c, resp)
}

// parsePathID parses a positive int64 from a path param. Empty / non-numeric
// / non-positive values all collapse to "invalid params" — the caller decides
// the HTTP status.
func parsePathID(s string) (int64, error) {
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, err
	}
	if id <= 0 {
		return 0, strconv.ErrRange
	}
	return id, nil
}
