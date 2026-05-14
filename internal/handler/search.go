// Package handler — search.go is the HTTP binding for full-text content search.
//
// Follows the same "thin handler" pattern as post.go:
//  1. Parse and validate the wire input (query-string here, not JSON body)
//  2. Call exactly one service method
//  3. Translate the service's response into HTTP via response.* helpers
//
// No auth identity is pulled: search is a public endpoint (PRD §7 F-S01
// AC-6 — unauthenticated users can search). The route is mounted without
// middleware.Auth; per-IP rate limiting is applied at the router instead
// (see cmd/server/main.go).
//
// Added in Step 7 (search module).
package handler

import (
	"cooking-platform/internal/model/dto"
	"cooking-platform/internal/service"
	"cooking-platform/pkg/errcode"
	"cooking-platform/pkg/response"

	"github.com/gin-gonic/gin"
)

// SearchHandler hosts the handler for GET /api/v1/search.
type SearchHandler struct {
	svc *service.SearchService
}

// NewSearchHandler constructs a SearchHandler.
func NewSearchHandler(svc *service.SearchService) *SearchHandler {
	return &SearchHandler{svc: svc}
}

// Search handles GET /api/v1/search?q=&scene_tag=&cursor=&size=.
//
// Binding note: ShouldBindQuery only fails here on a malformed scene_tag /
// size (e.g. scene_tag=99, size=-1) — those are genuine bad input → 400
// ErrInvalidParams. The keyword itself carries no binding tag; empty /
// whitespace / over-length keyword rules live in the service layer
// (PRD AC-2 rejects empty, AC-7 truncates over-length), and the service
// returns ErrSearchKeywordEmpty which FromError maps to its own 400.
func (h *SearchHandler) Search(c *gin.Context) {
	var q dto.SearchQuery
	if err := c.ShouldBindQuery(&q); err != nil {
		response.BadRequest(c, errcode.ErrInvalidParams)
		return
	}

	resp, err := h.svc.Search(c.Request.Context(), q)
	if err != nil {
		response.FromError(c, err)
		return
	}
	response.Success(c, resp)
}
