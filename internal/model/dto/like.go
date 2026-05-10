// Package dto — like.go defines request and response DTOs for the like module.
//
// The like module has no request body DTOs: post_id is a URL path param,
// user_id comes from the JWT (Auth middleware), nothing else is needed.
// All three endpoints (POST /like, DELETE /like, GET /like) share a single
// response shape so the front-end can update the UI uniformly.
//
// LikeResp design rationale:
//
//   - liked: the canonical "is this user currently in the like set" boolean.
//     POST /like → always true on success (idempotent re-likes also return true).
//     DELETE /like → always false on success (idempotent re-unlikes also return false).
//     GET /like → the actual current state.
//
//   - count: the post's current like count, sourced from Redis (like:cnt:{post_id}),
//     so the front-end can update the visible counter without a separate fetch.
//     Eventually consistent with posts.like_count (LikeConsumer batches 50/3s),
//     but the user-visible value is always fresh because it lives in Redis.
//
// Added in Step 5 (like module).
package dto

// LikeResp is the response payload for all three like endpoints.
//
// The shape is identical for like / unlike / status to keep front-end
// state-update logic trivial: parse once, reconcile UI from the same
// fields regardless of which verb was issued.
type LikeResp struct {
	Liked bool   `json:"liked"`
	Count uint32 `json:"count"`
}
