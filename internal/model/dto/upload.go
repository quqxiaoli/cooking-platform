// Package dto — upload.go defines the request/response DTOs for the
// image-upload module (Step 9).
//
// Wire-format invariants (consistent with post.go):
//   - All time fields are int64 UnixMilli.
//   - Nonce is an opaque string the client must round-trip; never parse it.
//   - Headers in PresignResp are a map<header_name, header_value> the
//     client MUST set when PUT-ing to UploadURL. Aliyun fails the upload
//     with 403 if Content-Type mismatches the signed header.
//
// Validation:
//   - filename — string ≤ 200; used only to derive the file extension when
//     the content_type ext mapping is ambiguous. The OSS abstraction
//     today derives ext purely from content_type — filename is forwarded
//     so future changes (preserve original name, etc.) don't require a
//     DTO migration.
//   - content_type — exactly one of three whitelisted MIME types. We do
//     NOT accept HEIC / GIF / SVG in MVP: HEIC needs server-side
//     conversion, GIF/SVG carry attack surfaces (animated payload size,
//     embedded scripts).
//   - size — 1..5 MiB DTO ceiling. Service layer re-checks against
//     cfg.OSS.MaxImageSize so env overrides take effect without touching
//     the wire format.
//   - purpose — avatar/cover/step; drives the object_key prefix.
//
// Added in Step 9.
package dto

// ── Presign ─────────────────────────────────────────────────────────────────

// PresignReq is the body of POST /api/v1/upload/presign.
type PresignReq struct {
	Filename    string `json:"filename"     binding:"required,max=200"`
	ContentType string `json:"content_type" binding:"required,oneof=image/jpeg image/png image/webp"`
	Size        int64  `json:"size"         binding:"required,min=1,max=5242880"` // 5 MiB
	Purpose     string `json:"purpose"      binding:"required,oneof=avatar cover step"`
}

// PresignResp tells the client where to PUT, what headers to use, and the
// nonce to round-trip when notifying us of upload completion.
//
// PublicURL is returned BEFORE the upload actually happens. The client MAY
// optimistically render this URL the moment the PUT succeeds — no need to
// await the callback round-trip just to know what URL to display.
type PresignResp struct {
	UploadURL string            `json:"upload_url"`
	PublicURL string            `json:"public_url"`
	Method    string            `json:"method"`
	Headers   map[string]string `json:"headers"`
	Nonce     string            `json:"nonce"`
	ExpiresAt int64             `json:"expires_at"` // UnixMilli
}

// ── Callback ────────────────────────────────────────────────────────────────

// CallbackReq is the body of POST /api/v1/upload/callback.
//
// We deliberately do NOT accept the public URL from the client: the server
// already knows it (saved alongside the nonce at presign time). Letting the
// client supply the URL would defeat the whitelist — a hostile client could
// post a third-party URL and have us record it as theirs. By making the
// server the sole source of truth, the only attack surface left is "can
// you guess someone else's nonce", which is bounded by 128 bits of entropy.
type CallbackReq struct {
	Nonce string `json:"nonce" binding:"required,min=16,max=128"`
}

// CallbackResp confirms persistence by echoing back the public URL that
// the client may now attach to profiles / posts.
//
// ObjectKey is included for client-side forensic logging — when an upload
// later fails image-format checks or audit (Step 10), the client can
// surface the object_key in a bug report.
type CallbackResp struct {
	URL       string `json:"url"`
	ObjectKey string `json:"object_key"`
}
