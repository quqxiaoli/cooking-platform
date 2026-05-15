// Package oss — client.go declares the Client interface plus all shared
// types and a factory selecting the concrete implementation from config.
package oss

import (
	"fmt"
	"time"

	"cooking-platform/pkg/config"
)

// PresignPurpose identifies what the upload will be used for. It drives the
// object_key prefix: avatar/, cover/, step/. Centralising it as a typed
// string prevents stringly-typed bugs at call sites and lets the validator
// reject unknown values at the DTO boundary.
type PresignPurpose string

const (
	PurposeAvatar PresignPurpose = "avatar"
	PurposeCover  PresignPurpose = "cover"
	PurposeStep   PresignPurpose = "step"
)

// IsValid reports whether p is one of the three known purposes.
func (p PresignPurpose) IsValid() bool {
	switch p {
	case PurposeAvatar, PurposeCover, PurposeStep:
		return true
	default:
		return false
	}
}

// PresignParams is the input to Client.Presign. The service layer constructs
// it after DTO validation, so every implementation can trust the values.
type PresignParams struct {
	UserID      int64          // owner; embedded in object_key for traceability
	Filename    string         // original client filename (used for ext fallback)
	ContentType string         // MIME — already whitelisted by DTO binding
	Size        int64          // declared content length, in bytes
	Purpose     PresignPurpose // avatar | cover | step
}

// PresignResult is what the handler returns to the client.
//
// UploadURL: the URL to PUT to (may include signed query params).
// PublicURL: the eventual public-facing URL of the uploaded object.
// Headers: any request headers the client MUST set when PUT-ing (typically
//
//	Content-Type for Aliyun's signature to match). Empty for mock.
//
// ObjectKey: persisted in the nonce record so the callback can re-derive
//
//	PublicURL without trusting the client.
//
// ExpiresAt: when UploadURL stops being honoured.
type PresignResult struct {
	UploadURL string
	PublicURL string
	Method    string
	Headers   map[string]string
	ObjectKey string
	ExpiresAt time.Time
}

// Client is the contract every OSS implementation satisfies.
//
// Two methods only: Presign (the operational entry point) and Close
// (lifecycle hook). VerifyCallback is intentionally absent — callback
// authenticity is enforced via Redis nonce in upload_service.go, not by
// per-implementation signature schemes. That keeps the abstraction lean
// and the dev/prod paths behaviourally identical.
type Client interface {
	// Presign builds a short-lived URL the client uses to PUT one image
	// to OSS. Implementations MUST embed a TTL ≤ cfg.OSS.PresignTTL into
	// the URL itself so an expired URL cannot be replayed.
	Presign(params PresignParams) (*PresignResult, error)

	// Close releases the implementation's resources. Safe to call even
	// when New<Impl> returned an error; safe to call multiple times.
	Close() error
}

// NewClient is the factory invoked from main.go.
//
// Provider=mock — returns a MockClient that hosts a local HTTP listener
//
//	so verify_step9.sh can run the full upload chain.
//
// Provider=aliyun — returns an AliyunClient backed by the real SDK.
// Unknown providers fail loudly at startup (so misconfiguration never
// silently lands in production).
func NewClient(cfg config.OSSConfig) (Client, error) {
	switch cfg.Provider {
	case "mock", "":
		return NewMockClient(cfg)
	case "aliyun":
		return NewAliyunClient(cfg)
	default:
		return nil, fmt.Errorf("unknown oss provider: %q (supported: mock, aliyun)", cfg.Provider)
	}
}

// extFromContentType maps an image MIME to a filename extension.
// Centralised so mock and aliyun derive identical object_keys for the
// same input.
func extFromContentType(ct string) string {
	switch ct {
	case "image/jpeg":
		return "jpg"
	case "image/png":
		return "png"
	case "image/webp":
		return "webp"
	default:
		return "bin" // factory + DTO validator should already prevent this
	}
}
