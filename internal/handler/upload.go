// Package handler — upload.go handles HTTP routing for the image-upload
// module (Step 9).
//
// Two endpoints, both behind middleware.Auth:
//
//	POST /api/v1/upload/presign  — issue a short-lived PUT URL.
//	POST /api/v1/upload/callback — confirm the upload, return the
//	                               canonical public URL.
//
// Handlers stay thin: parse → call service → respond. Same convention as
// user.go / post.go.
//
// Added in Step 9.
package handler

import (
	"cooking-platform/internal/middleware"
	"cooking-platform/internal/model/dto"
	"cooking-platform/internal/service"
	"cooking-platform/pkg/errcode"
	"cooking-platform/pkg/response"

	"github.com/gin-gonic/gin"
)

// UploadHandler holds the dependencies needed by all upload-related routes.
type UploadHandler struct {
	svc *service.UploadService
}

// NewUploadHandler constructs an UploadHandler.
func NewUploadHandler(svc *service.UploadService) *UploadHandler {
	return &UploadHandler{svc: svc}
}

// Presign handles POST /api/v1/upload/presign. Requires Auth middleware.
//
// Returns: { upload_url, public_url, method, headers, nonce, expires_at }.
// Client must PUT the image bytes to upload_url with the supplied headers
// before the nonce expires (cfg.OSS.PresignTTL = 15min by default), then
// notify the server via POST /upload/callback.
func (h *UploadHandler) Presign(c *gin.Context) {
	uid := middleware.GetUserID(c)
	if uid == 0 {
		response.FromError(c, errcode.ErrUnauthorized)
		return
	}

	var req dto.PresignReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.FromError(c, errcode.ErrInvalidParams)
		return
	}

	resp, err := h.svc.Presign(c.Request.Context(), uid, req)
	if err != nil {
		response.FromError(c, err)
		return
	}
	response.Success(c, resp)
}

// Callback handles POST /api/v1/upload/callback. Requires Auth middleware.
//
// Client posts the nonce it received from Presign once it has finished
// PUT-ing bytes to OSS. We atomically consume the nonce (GETDEL) and
// return the canonical public URL — server-side bookkeeping that ties one
// upload to one logged-in user.
func (h *UploadHandler) Callback(c *gin.Context) {
	uid := middleware.GetUserID(c)
	if uid == 0 {
		response.FromError(c, errcode.ErrUnauthorized)
		return
	}

	var req dto.CallbackReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.FromError(c, errcode.ErrInvalidParams)
		return
	}

	resp, err := h.svc.Callback(c.Request.Context(), uid, req)
	if err != nil {
		response.FromError(c, err)
		return
	}
	response.Success(c, resp)
}
