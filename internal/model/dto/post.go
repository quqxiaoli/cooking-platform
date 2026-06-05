// Package dto — post.go defines request and response DTOs for the content module.
//
// DTOs are the wire format between client and server. Keep them stable —
// breaking changes here break clients. New fields should be added with
// `omitempty` so older clients ignoring them still work.
//
// Field-level decisions:
//
//   - SceneTag is exposed as int8 (not the model.SceneTag typed alias)
//     because the DTO package must not depend on internal/model — that
//     would couple wire format to domain types and create import cycles
//     in larger projects. Service layer converts at the boundary.
//
//   - All time fields are int64 UnixMilli, not time.Time. Two reasons:
//     1. Avoids JS timezone-parsing pitfalls (Date(string) is impl-specific).
//     2. Smaller, more predictable JSON output ("created_at": 1714200000000).
//     Frontend converts to local time with new Date(ms).
//
//   - Cursor is an opaque string carrying decimal milliseconds today.
//     Clients MUST NOT parse the cursor; they pass it back verbatim.
//     This lets us migrate to base64 / signed cursors later without breaking.
//
//   - has_more + next_cursor both signal "more pages". Redundant on purpose —
//     a brittle client checking next_cursor != "" still works alongside
//     one that just trusts has_more. Cheap insurance.
//
// [Step 9] Added structured steps:
//   - CreatePostReq.Steps  — optional, 0..30 entries (empty = legacy text-only)
//   - PostDetailResp.Steps — same shape, empty means the post predates
//     structured steps; client falls back to Content.
//
// Future improvements:
//   - Add cook_duration to CreatePostReq when frontend exposes the picker.
//   - Add secondary_tags []int8 when multi-select tag UI ships (PRD §9.2).
//   - Sign cursors with HMAC once paginated views expose private content
//     (drafts, audited-out posts) where tampering must not skip ACL.
//
// Added in Step 4 (content module). Extended in Step 9 (structured steps).
package dto

// ── Requests ────────────────────────────────────────────────────────────────

// CreatePostReq is the body of POST /api/v1/posts.
//
// Validation rules (enforced via go-playground/validator):
//
//   - title: required, 1..100 chars. Frontend should pre-trim whitespace;
//     backend re-validates because we never trust the client.
//   - scene_tag: required, 1..8. The 8 canonical scenes; service layer
//     re-checks via model.SceneTag.IsValid() to handle the case where
//     someone disables binding tags via library upgrade.
//   - content: optional, up to 5000 chars. When Steps is empty this is
//     the body of a legacy text-only post; when Steps is non-empty it
//     acts as an optional summary line above the structured steps.
//   - cover_url: optional, ≤ 500 chars. Service layer rejects URLs that
//     don't pass the OSS whitelist (Step 9).
//   - steps: optional, 0..30 entries. Empty = legacy text-only post.
//     Each step has text (1..500 chars, required) and 0..3 image URLs
//     (each ≤ 500 chars). Service layer also whitelists the URLs.
type CreatePostReq struct {
	Title    string        `json:"title"     binding:"required,min=1,max=100"`
	SceneTag int8          `json:"scene_tag" binding:"required,min=1,max=8"`
	Content  string        `json:"content"   binding:"max=5000"`
	CoverURL string        `json:"cover_url" binding:"omitempty,max=500"`
	Steps    []PostStepReq `json:"steps"     binding:"omitempty,max=30,dive"`
}

// PostStepReq is one step in CreatePostReq.Steps.
//
// The "required,max=500" tag on Text means a step with empty text is
// rejected — even when image_urls is non-empty. PRD-Phase2 §F-C01 mandates
// each step have at least a description; a step that's just images with no
// caption isn't a step, it's a gallery.
type PostStepReq struct {
	Text      string   `json:"text"       binding:"required,max=500"`
	ImageURLs []string `json:"image_urls" binding:"omitempty,max=3,dive,max=500"`
}

// FeedQuery is the query-string struct for GET /api/v1/feed.
//
// scene_tag = 0 (or absent) → no scene filter (全部 Feed).
// cursor    = ""             → first page.
// cursor    = "<unix_milli>" → return items strictly older than that ms timestamp.
// size: 1..50, defaults to 20 in service when 0.
type FeedQuery struct {
	SceneTag int8   `form:"scene_tag" binding:"omitempty,min=1,max=8"`
	Cursor   string `form:"cursor"`
	Size     int    `form:"size"      binding:"omitempty,min=1,max=50"`
}

// ── Responses ───────────────────────────────────────────────────────────────

// CreatePostResp is returned after a successful POST /api/v1/posts.
type CreatePostResp struct {
	PostID      int64 `json:"post_id"`
	AuditStatus uint8 `json:"audit_status"`
	IsVisible   uint8 `json:"is_visible"`
	CreatedAt   int64 `json:"created_at"` // UnixMilli
}

// PostAuthorBrief is the embedded author snapshot used in feed cards and
// detail pages.
type PostAuthorBrief struct {
	ID        int64  `json:"id"`
	Nickname  string `json:"nickname"`
	AvatarURL string `json:"avatar_url"`
}

// PostListItem is one row in any feed/list response (homepage feed,
// scene-filtered feed, author-page feed). Excludes Content because feed
// cards render only title + cover.
//
// LikedByMe reflects whether the authenticated caller (if any) has liked
// this post. Anonymous callers always see false. Patched per-page by
// AttachLikedByMe after the base list is built so the feed cache stays
// viewer-agnostic.
type PostListItem struct {
	ID        int64           `json:"id"`
	Title     string          `json:"title"`
	SceneTag  int8            `json:"scene_tag"`
	SceneName string          `json:"scene_name"`
	CoverURL  string          `json:"cover_url"`
	LikeCount uint32          `json:"like_count"`
	ViewCount uint32          `json:"view_count"`
	Author    PostAuthorBrief `json:"author"`
	CreatedAt int64           `json:"created_at"`  // UnixMilli
	LikedByMe bool            `json:"liked_by_me"` // viewer has liked this post
}

// PostStepResp is one step in PostDetailResp.Steps. Mirrors PostStepReq
// without the binding tags. Always serialises ImageURLs as a JSON array
// (even when empty) so frontends can range without nil checks.
type PostStepResp struct {
	StepNo    uint8    `json:"step_no"`
	Text      string   `json:"text"`
	ImageURLs []string `json:"image_urls"`
}

// PostDetailResp is the full detail view returned by GET /api/v1/posts/:id.
//
// Steps is an empty array for legacy text-only posts (created via Step 4
// MVP before the post_steps subtable existed, or via Step 9+ clients that
// prefer the content-only flow). Clients should fall back to rendering
// Content when Steps is empty.
type PostDetailResp struct {
	ID          int64           `json:"id"`
	Title       string          `json:"title"`
	SceneTag    int8            `json:"scene_tag"`
	SceneName   string          `json:"scene_name"`
	Content     string          `json:"content"`
	CoverURL    string          `json:"cover_url"`
	LikeCount   uint32          `json:"like_count"`
	ViewCount   uint32          `json:"view_count"`
	Author      PostAuthorBrief `json:"author"`
	Steps       []PostStepResp  `json:"steps"`
	AuditStatus uint8           `json:"audit_status"`
	IsVisible   uint8           `json:"is_visible"`
	CreatedAt   int64           `json:"created_at"` // UnixMilli
	UpdatedAt   int64           `json:"updated_at"` // UnixMilli
}

// FeedResp wraps a cursor-paginated feed page.
type FeedResp struct {
	Posts      []PostListItem `json:"posts"`
	NextCursor string         `json:"next_cursor"` // "" when no more
	HasMore    bool           `json:"has_more"`
}
