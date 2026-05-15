// Package service — upload_service.go orchestrates the image-upload flow:
//
//	Presign  — accept upload intent, ask oss.Client for a presigned URL,
//	           save nonce → record in Redis with cfg.OSS.PresignTTL TTL,
//	           return everything the client needs to PUT the bytes.
//
//	Callback — atomically consume the nonce (GETDEL), verify the caller
//	           owns it, return the recorded public URL.
//
// The service is dependency-injected with the oss.Client interface so the
// dev (MockClient with embedded HTTP listener) and prod (AliyunClient with
// SignURL) paths share zero behavioural code in this file.
//
// Security model:
//   - Auth middleware ensures only logged-in users hit either endpoint.
//   - Presign rate limit (PerUserKey, 30/hour) gates spam — wired at
//     the route level in main.go.
//   - Nonce record holds the canonical user_id; Callback rejects when the
//     caller's user_id doesn't match, so a leaked nonce cannot be used
//     by a different account.
//   - The PublicURL is server-constructed and stored in the nonce record;
//     the callback returns it from there, never accepts it from the client.
//     Defence in depth is layered on top by UserService / PostService
//     re-running oss.IsAllowedURL when the URL is attached to a resource.
//
// Added in Step 9 (image upload module).
package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"

	"cooking-platform/internal/cache"
	"cooking-platform/internal/model/dto"
	"cooking-platform/pkg/config"
	"cooking-platform/pkg/errcode"
	"cooking-platform/pkg/oss"

	"go.uber.org/zap"
)

// UploadService is the entry point for all image-upload operations.
type UploadService struct {
	ossClient   oss.Client
	uploadCache *cache.UploadCache
	ossCfg      config.OSSConfig
}

// NewUploadService wires the service with its dependencies.
func NewUploadService(
	ossClient oss.Client,
	uploadCache *cache.UploadCache,
	ossCfg config.OSSConfig,
) *UploadService {
	return &UploadService{
		ossClient:   ossClient,
		uploadCache: uploadCache,
		ossCfg:      ossCfg,
	}
}

// Presign asks oss.Client for a short-lived PUT URL, records a nonce in
// Redis, and returns the upload package to the client.
//
// DTO binding has already validated content_type, size, and purpose. We
// re-check size against cfg here because config can override the DTO's
// hard-coded 5 MiB ceiling — env overrides must take effect without
// touching the wire format.
func (s *UploadService) Presign(ctx context.Context, userID int64, req dto.PresignReq) (*dto.PresignResp, error) {
	if req.Size > s.ossCfg.MaxImageSize {
		return nil, errcode.ErrUploadFileTooLarge
	}

	purpose := oss.PresignPurpose(req.Purpose)
	if !purpose.IsValid() {
		// DTO binding's oneof guards this, but defending again is cheap.
		return nil, errcode.ErrInvalidParams
	}

	result, err := s.ossClient.Presign(oss.PresignParams{
		UserID:      userID,
		Filename:    req.Filename,
		ContentType: req.ContentType,
		Size:        req.Size,
		Purpose:     purpose,
	})
	if err != nil {
		zap.L().Error("oss presign failed",
			zap.Int64("user_id", userID),
			zap.String("purpose", req.Purpose),
			zap.Error(err),
		)
		return nil, errcode.ErrUploadPresignFailed
	}

	nonce, err := generateNonce()
	if err != nil {
		zap.L().Error("generate nonce failed", zap.Error(err))
		return nil, errcode.ErrUploadPresignFailed
	}

	rec := cache.NonceRecord{
		UserID:      userID,
		ObjectKey:   result.ObjectKey,
		PublicURL:   result.PublicURL,
		Purpose:     req.Purpose,
		ContentType: req.ContentType,
		Size:        req.Size,
	}
	if err := s.uploadCache.SaveNonce(ctx, nonce, rec, s.ossCfg.PresignTTL); err != nil {
		zap.L().Error("save nonce failed",
			zap.Int64("user_id", userID),
			zap.Error(err),
		)
		return nil, errcode.ErrUploadPresignFailed
	}

	return &dto.PresignResp{
		UploadURL: result.UploadURL,
		PublicURL: result.PublicURL,
		Method:    result.Method,
		Headers:   result.Headers,
		Nonce:     nonce,
		ExpiresAt: result.ExpiresAt.UnixMilli(),
	}, nil
}

// Callback atomically consumes the nonce and returns the canonical public URL.
//
// Ownership check: the nonce record contains the user_id of the presign
// caller. The callback's userID (from Auth middleware) must match — this
// prevents a stolen nonce from being used to attach an upload to a
// different account.
//
// Atomicity: ConsumeNonce uses GETDEL so two simultaneous callbacks for
// the same nonce see one success and one ErrCacheNotFound. The losing
// caller gets ErrUploadCallbackInvalid (460104), which matches what they'd
// see if the nonce had expired — uniform error response avoids leaking
// which case fired.
func (s *UploadService) Callback(ctx context.Context, userID int64, req dto.CallbackReq) (*dto.CallbackResp, error) {
	rec, err := s.uploadCache.ConsumeNonce(ctx, req.Nonce)
	if err != nil {
		if errors.Is(err, cache.ErrCacheNotFound) {
			return nil, errcode.ErrUploadCallbackInvalid
		}
		zap.L().Error("consume nonce failed", zap.Error(err))
		return nil, errcode.ErrServer
	}
	if rec.UserID != userID {
		// Nonce ownership mismatch — log (someone may be probing) and
		// surface the same generic error as missing-nonce.
		zap.L().Warn("upload nonce ownership mismatch",
			zap.Int64("expected_user_id", rec.UserID),
			zap.Int64("actual_user_id", userID),
		)
		return nil, errcode.ErrUploadCallbackInvalid
	}

	return &dto.CallbackResp{
		URL:       rec.PublicURL,
		ObjectKey: rec.ObjectKey,
	}, nil
}

// generateNonce produces a 32-character hex string from 16 crypto-random
// bytes (128 bits of entropy). Collision-free at any realistic deployment
// scale and short enough to round-trip via JSON without further encoding.
func generateNonce() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("crypto/rand: %w", err)
	}
	return hex.EncodeToString(buf), nil
}
