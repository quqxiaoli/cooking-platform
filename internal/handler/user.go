// Package handler — user.go handles HTTP routing for the user module.
//
// Handlers are thin: they parse request, call service, and dispatch the
// response. No business logic, no infrastructure access. All errors are
// routed through response.FromError so AppError values get the correct
// HTTP status automatically.
//
// Step 3 (user module).
package handler

import (
	"errors"
	"strconv"

	"cooking-platform/internal/middleware"
	"cooking-platform/internal/model/dto"
	"cooking-platform/internal/service"
	"cooking-platform/pkg/errcode"
	"cooking-platform/pkg/response"

	"github.com/gin-gonic/gin"
)

// UserHandler holds the dependencies needed by all user-related routes.
type UserHandler struct {
	svc *service.UserService
}

// NewUserHandler constructs a UserHandler.
func NewUserHandler(svc *service.UserService) *UserHandler {
	return &UserHandler{svc: svc}
}

// SendCode handles POST /api/v1/auth/send-code.
func (h *UserHandler) SendCode(c *gin.Context) {
	var req dto.SendCodeReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.FromError(c, errcode.ErrInvalidParams)
		return
	}

	resp, err := h.svc.SendCode(c.Request.Context(), req.Phone, c.ClientIP())
	if err != nil {
		response.FromError(c, err)
		return
	}
	response.Success(c, resp)
}

// Login handles POST /api/v1/auth/login.
func (h *UserHandler) Login(c *gin.Context) {
	var req dto.LoginReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.FromError(c, errcode.ErrInvalidParams)
		return
	}

	resp, err := h.svc.Login(c.Request.Context(), req.Phone, req.Code)
	if err != nil {
		response.FromError(c, err)
		return
	}
	response.Success(c, resp)
}

// Refresh handles POST /api/v1/auth/refresh.
func (h *UserHandler) Refresh(c *gin.Context) {
	var req dto.RefreshReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.FromError(c, errcode.ErrInvalidParams)
		return
	}

	pair, err := h.svc.Refresh(c.Request.Context(), req.RefreshToken)
	if err != nil {
		response.FromError(c, err)
		return
	}
	response.Success(c, pair)
}

// Logout handles POST /api/v1/auth/logout. Requires Auth middleware.
//
// We re-extract the token from the header (rather than passing the JTI from
// context) because we need the full token to determine its remaining TTL
// for accurate blacklist expiry.
func (h *UserHandler) Logout(c *gin.Context) {
	token, err := bearerTokenOrEmpty(c.GetHeader("Authorization"))
	if err != nil {
		response.FromError(c, errcode.ErrUnauthorized)
		return
	}
	if logoutErr := h.svc.Logout(c.Request.Context(), token); logoutErr != nil {
		response.FromError(c, logoutErr)
		return
	}
	response.Success(c, nil)
}

// GetMyProfile handles GET /api/v1/users/me. Requires Auth middleware.
func (h *UserHandler) GetMyProfile(c *gin.Context) {
	uid := middleware.GetUserID(c)
	if uid == 0 {
		response.FromError(c, errcode.ErrUnauthorized)
		return
	}
	profile, err := h.svc.GetMyProfile(c.Request.Context(), uid)
	if err != nil {
		response.FromError(c, err)
		return
	}
	response.Success(c, profile)
}

// GetPublicProfile handles GET /api/v1/users/:id. Public endpoint with
// optional-auth: when a valid Bearer token is present the response carries
// is_following resolved against the follow table; otherwise it stays false.
func (h *UserHandler) GetPublicProfile(c *gin.Context) {
	idStr := c.Param("id")
	id, parseErr := strconv.ParseInt(idStr, 10, 64)
	if parseErr != nil || id <= 0 {
		response.FromError(c, errcode.ErrInvalidParams)
		return
	}
	viewerID := middleware.GetUserID(c) // 0 = anonymous (OptionalAuth)
	profile, err := h.svc.GetPublicProfile(c.Request.Context(), viewerID, id)
	if err != nil {
		response.FromError(c, err)
		return
	}
	response.Success(c, profile)
}

// UpdateProfile handles PATCH /api/v1/users/me. Requires Auth middleware.
func (h *UserHandler) UpdateProfile(c *gin.Context) {
	uid := middleware.GetUserID(c)
	if uid == 0 {
		response.FromError(c, errcode.ErrUnauthorized)
		return
	}
	var req dto.UpdateProfileReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.FromError(c, errcode.ErrInvalidParams)
		return
	}
	if err := h.svc.UpdateProfile(c.Request.Context(), uid, req); err != nil {
		response.FromError(c, err)
		return
	}
	response.Success(c, nil)
}

// errInvalidAuth is the sentinel returned when the Authorization header
// cannot be parsed as a Bearer token.
var errInvalidAuth = errors.New("invalid auth header")

// bearerTokenOrEmpty is a relaxed parser used by handlers that need the raw
// token (vs. middleware which already validated). Returns the token portion
// or an error.
func bearerTokenOrEmpty(header string) (string, error) {
	const prefix = "Bearer "
	if len(header) <= len(prefix) {
		return "", errInvalidAuth
	}
	// Tolerate lowercase scheme.
	scheme := header[:len(prefix)]
	for i := 0; i < len(scheme); i++ {
		c := scheme[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != "bearer "[i] {
			return "", errInvalidAuth
		}
	}
	return header[len(prefix):], nil
}
