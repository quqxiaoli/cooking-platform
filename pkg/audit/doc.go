// Package audit wraps the Aliyun Content Security API.
// Used for text and image moderation at post-creation time.
// Results are delivered asynchronously via MQ (audit.q → AuditConsumer).
// Added in Step 10 (content moderation module).
package audit
