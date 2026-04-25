// Package middleware — ratelimit.go will implement Redis sliding-window rate limiting.
// Applied per-IP on public endpoints and per-user on write endpoints.
// Added in Step 3 (user module) for SMS send-code protection.
package middleware
