// Package audit wraps content-moderation providers behind a single Auditor
// interface. Follows the same three-piece pattern as pkg/sms and pkg/oss:
//
//   - auditor.go  — Auditor interface, ReviewRequest/ReviewResult types, NewAuditor factory
//   - mock.go     — MockAuditor for dev/test (verdict controlled by config)
//   - aliyun.go   — AliyunAuditor calling Aliyun Green content-safety service
//
// AuditConsumer (internal/consumer/audit_consumer.go) is the only caller.
// Service and handler layers never import this package directly.
//
// Added in Step 10 (content moderation).
package audit
