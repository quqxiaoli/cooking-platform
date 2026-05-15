// Package oss is the cooking-platform's image-upload abstraction. It exposes
// a single Client interface with two production-relevant operations:
//
//	Presign — issues a short-lived URL the client uses to PUT image bytes
//	          directly to OSS. The Go server is never on the upload path.
//	Close   — releases any resources held by the implementation. The Aliyun
//	          implementation is a no-op; MockClient shuts down its embedded
//	          HTTP server.
//
// Implementations:
//
//	client.go    — Client interface + shared types + NewClient factory
//	mock.go      — MockClient. Spins up a local HTTP listener that accepts
//	               PUT /<object_key> requests, so verify_step9.sh can run
//	               the full presign → PUT → callback chain in dev without
//	               touching Aliyun.
//	aliyun.go    — AliyunClient. Uses the aliyun-oss-go-sdk Bucket.SignURL
//	               method to issue presigned PUT URLs. Caller hits OSS
//	               directly; the Go server never relays bytes.
//	whitelist.go — IsAllowedURL helper. Used by user_service /
//	               post_service to reject avatar / cover / step image URLs
//	               that aren't on our OSS host (defence in depth — a
//	               compromised client cannot redirect to a third-party host).
//
// ── Why presigned URLs, not STS tokens ────────────────────────────────────
//
// PRD-Phase3 §8 describes STS temporary credentials. We chose presigned
// PUT URLs instead at Step 9 implementation time:
//
//   - No per-presign STS API call (lower latency, lower cost).
//   - Client uses a plain HTTP PUT — no OSS SDK on the frontend.
//   - Permission granularity stays sufficient: each presigned URL is
//     scoped to one fixed object_key with a 15-minute TTL.
//
// This is a deliberate, registered PRD deviation; see the Step 9 偏离点
// section in the project progress doc.
//
// Added in Step 9 (image upload module).
package oss
