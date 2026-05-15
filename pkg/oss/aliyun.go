// Package oss — aliyun.go is the production implementation backed by the
// official Aliyun OSS Go SDK. It signs PUT URLs via Bucket.SignURL so the
// client uploads directly to OSS without routing bytes through Go.
//
// The signed URL embeds:
//   - the requested HTTP method (PUT),
//   - the object_key path,
//   - the TTL (in seconds),
//   - the Content-Type the client must use,
//   - an HMAC-SHA1 signature over the canonical string.
//
// If the client tampers with any of these — wrong key, wrong content-type,
// expired clock — OSS rejects the PUT with 403. That gives us object-level
// authorisation without ever brokering a long-lived AccessKey to the
// frontend.
package oss

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"cooking-platform/pkg/config"

	alioss "github.com/aliyun/aliyun-oss-go-sdk/oss"
	"go.uber.org/zap"
)

// AliyunClient is a thin adapter around alioss.Bucket.
type AliyunClient struct {
	cfg    config.OSSConfig
	bucket *alioss.Bucket
	log    *zap.Logger
}

// NewAliyunClient validates required config and dials the SDK.
//
// We fail fast on missing AccessKey / Endpoint / Bucket / URLPrefix to
// avoid the standard "production-only" landmine: a misconfigured prod
// build silently returning broken presigned URLs.
func NewAliyunClient(cfg config.OSSConfig) (*AliyunClient, error) {
	if cfg.AccessKeyID == "" || cfg.AccessKeySecret == "" {
		return nil, fmt.Errorf("aliyun oss: access key id/secret required")
	}
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("aliyun oss: endpoint required (e.g. oss-cn-beijing.aliyuncs.com)")
	}
	if cfg.Bucket == "" {
		return nil, fmt.Errorf("aliyun oss: bucket required")
	}
	if cfg.URLPrefix == "" {
		return nil, fmt.Errorf("aliyun oss: url_prefix required (whitelist baseline)")
	}

	client, err := alioss.New(cfg.Endpoint, cfg.AccessKeyID, cfg.AccessKeySecret)
	if err != nil {
		return nil, fmt.Errorf("aliyun oss: new client: %w", err)
	}
	bucket, err := client.Bucket(cfg.Bucket)
	if err != nil {
		return nil, fmt.Errorf("aliyun oss: open bucket %q: %w", cfg.Bucket, err)
	}

	return &AliyunClient{
		cfg:    cfg,
		bucket: bucket,
		log:    zap.L().Named("oss.aliyun"),
	}, nil
}

// Presign issues a short-lived PUT URL.
//
// SignURL TTL is in seconds; we pin it to cfg.PresignTTL (default 15m).
// alioss.ContentType(...) binds the Content-Type into the signed string so
// a tampered request from the client (different MIME) fails server-side.
func (c *AliyunClient) Presign(params PresignParams) (*PresignResult, error) {
	if !params.Purpose.IsValid() {
		return nil, fmt.Errorf("invalid purpose: %q", params.Purpose)
	}
	objectKey := buildObjectKey(params)
	ttl := c.cfg.PresignTTL
	if ttl <= 0 {
		ttl = 15 * time.Minute
	}

	uploadURL, err := c.bucket.SignURL(
		objectKey,
		alioss.HTTPPut,
		int64(ttl.Seconds()),
		alioss.ContentType(params.ContentType),
	)
	if err != nil {
		return nil, fmt.Errorf("sign put url: %w", err)
	}

	return &PresignResult{
		UploadURL: uploadURL,
		PublicURL: strings.TrimRight(c.cfg.URLPrefix, "/") + "/" + objectKey,
		Method:    http.MethodPut,
		Headers: map[string]string{
			// MUST match the alioss.ContentType option above or OSS will 403.
			"Content-Type": params.ContentType,
		},
		ObjectKey: objectKey,
		ExpiresAt: time.Now().Add(ttl),
	}, nil
}

// Close is a no-op for AliyunClient — the underlying SDK uses HTTP and
// nothing needs explicit teardown. Kept for interface symmetry with
// MockClient (which DOES own a listener).
func (c *AliyunClient) Close() error {
	return nil
}
