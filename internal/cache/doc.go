// Package cache contains Redis operation wrappers organised by domain.
// Cache functions must not contain business logic — they are pure read/write helpers.
// cache/feed.go  — Feed version key (cursor invalidation)
// cache/like.go  — Like status SADD/SISMEMBER/SREM
// cache/pv.go    — Page-view deduplication (HyperLogLog)
// cache/session.go — JWT blacklist + SMS code storage
// cache/sms.go   — SMS rate limiting (send window + daily cap)
// cache/bloom.go — Bloom filter for hot-post like dedup (hot-path degradation)
package cache
