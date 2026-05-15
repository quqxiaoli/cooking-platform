// Package dto — follow.go defines request and response DTOs for the follow
// module (PRD-Phase2 §8 F-F01).
//
// DTOs are the wire format between client and server. Keep them stable —
// breaking changes here break clients.
//
// Field-level decisions:
//
//   - All time-free here: follow actions return only a boolean state, and
//     follow lists render user briefs that carry no timestamps. There is
//     deliberately no "followed_at" on the list rows — the product shows
//     "头像 + 昵称" only (PRD §8), and exposing the follows.id-derived
//     ordering as a visible date would invite clients to sort on it.
//
//   - Cursor is an opaque decimal string (follows.id under the hood).
//     Clients MUST NOT parse it; they pass it back verbatim. Same contract
//     as the feed module's cursor — leaves room to migrate to signed /
//     base64 cursors later without breaking clients.
//
//   - has_more + next_cursor both signal "more pages", redundantly on
//     purpose: a brittle client checking `next_cursor != ""` still works
//     alongside one that trusts `has_more`. Cheap insurance, consistent
//     with FeedResp.
//
// Added in Step 8 (follow module).
package dto

// ── Requests ────────────────────────────────────────────────────────────────

// FollowListQuery is the query-string struct for
// GET /api/v1/users/:id/followers and /following.
//
//	cursor = ""            → first page.
//	cursor = "<follows.id>" → return rows older than that follow edge.
//	size: 1..50, service defaults to 20 when 0 (param absent).
//
// The target user id is a path parameter (:id), not part of this struct —
// the handler parses it separately, mirroring how FeedQuery omits the
// path-bound author id.
type FollowListQuery struct {
	Cursor string `form:"cursor"`
	Size   int    `form:"size" binding:"omitempty,min=1,max=50"`
}

// ── Responses ───────────────────────────────────────────────────────────────

// FollowActionResp is returned by POST and DELETE /api/v1/users/:id/follow.
//
// Following is the resulting relationship state from the caller's point of
// view: true after a successful (or idempotent) follow, false after a
// successful unfollow. The frontend toggles the "关注 / 已关注" button on
// this single field (PRD §8 AC-4) — no need to echo back counts, the
// profile page re-reads those from GET /users/:id.
type FollowActionResp struct {
	Following bool `json:"following"`
}

// UserBrief is one row in a follower / following list: just enough to render
// a tappable list entry (PRD §8: "头像 + 昵称，可点击跳转对方主页").
//
// Counters (follower_count etc.) are deliberately excluded — those are
// profile-page concerns fetched via GET /users/:id. Keeping this struct
// lean matters: a 50-row following list would otherwise duplicate 4 counter
// ints per row for no UI benefit.
//
// Fields mirror PostAuthorBrief exactly (id / nickname / avatar_url). They
// are kept as a separate type rather than reusing PostAuthorBrief because
// the semantic context differs — a follow-list row is a user, not a post's
// author — and coupling the follow wire format to a post-flavoured struct
// name would be a refactor hazard. The cosmetic duplication is the lesser
// evil; Step 12's DTO consolidation can revisit it.
type UserBrief struct {
	ID        int64  `json:"id"`
	Nickname  string `json:"nickname"`
	AvatarURL string `json:"avatar_url"`
}

// FollowListResp wraps a cursor-paginated page of a follower / following list.
//
// Cursor encoding: decimal string of the last row's follows.id. To fetch the
// next page, pass it back unchanged as the `cursor` query param. Empty
// next_cursor (and has_more=false) signals end-of-list.
type FollowListResp struct {
	Users      []UserBrief `json:"users"`
	NextCursor string      `json:"next_cursor"` // "" when no more
	HasMore    bool        `json:"has_more"`
}
