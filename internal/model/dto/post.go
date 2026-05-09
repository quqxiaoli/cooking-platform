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
// Future improvements:
//   - Add cook_duration to CreatePostReq when frontend exposes the picker.
//   - Add secondary_tags []int8 when multi-select tag UI ships (PRD §9.2).
//   - Sign cursors with HMAC once paginated views expose private content
//     (drafts, audited-out posts) where tampering must not skip ACL.
//
// Added in Step 4 (content module).
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
//   - content: optional, up to 5000 chars for the MVP text-only version.
//     5000 = ~8-10 minutes of reading; longer recipes will be wanted only
//     after structured post_steps lands (Step 9).
//   - cover_url: optional, <= 500 chars when present. Step 4 accepts any
//     string; Step 9 will tighten to OSS-host whitelist.
type CreatePostReq struct {
	Title    string `json:"title"     binding:"required,min=1,max=100"`
	SceneTag int8   `json:"scene_tag" binding:"required,min=1,max=8"`
	Content  string `json:"content"   binding:"max=5000"`
	CoverURL string `json:"cover_url" binding:"omitempty,max=500"`
}

// FeedQuery is the query-string struct for GET /api/v1/feed.
//
// scene_tag = 0 (or absent) → no scene filter (全部 Feed).
// cursor    = ""             → first page.
// cursor    = "<unix_milli>" → return items strictly older than that ms timestamp.
// size: 1..50, defaults to 20 in service when 0.
//
// Why `binding:"omitempty,..."` on scene_tag:
//   - Default int8 is 0; without omitempty, validator would fail on every
//     request that omits the param.
//   - Service rejects scene_tag values outside 0..8 explicitly, so the
//     binding=8 max is a defensive net only.
type FeedQuery struct {
	SceneTag int8   `form:"scene_tag" binding:"omitempty,min=1,max=8"`
	Cursor   string `form:"cursor"`
	Size     int    `form:"size"      binding:"omitempty,min=1,max=50"`
}

// ── Responses ───────────────────────────────────────────────────────────────

// CreatePostResp is returned after a successful POST /api/v1/posts.
//
// Contains the new post's id and its visibility/audit state so the client
// can decide what to show:
//   - MVP (Step 4): is_visible=1 immediately → "发布成功，已上线"
//   - Step 10+:    is_visible=0 typically   → "发布成功，正在审核"
type CreatePostResp struct {
	PostID      int64 `json:"post_id"`
	AuditStatus uint8 `json:"audit_status"`
	IsVisible   uint8 `json:"is_visible"`
	CreatedAt   int64 `json:"created_at"` // UnixMilli
}

// PostAuthorBrief is the embedded author snapshot used in feed cards and
// detail pages.
//
// We deliberately exclude follower/following/post counts here — those are
// profile-page concerns. Keeping this struct lean matters for Feed cache
// payload size: 20 posts * 4 redundant counter fields = 80 ints we'd
// otherwise duplicate per cache entry.
type PostAuthorBrief struct {
	ID        int64  `json:"id"`
	Nickname  string `json:"nickname"`
	AvatarURL string `json:"avatar_url"`
}

// PostListItem is one row in any feed/list response (homepage feed,
// scene-filtered feed, author-page feed). Excludes Content because feed
// cards render only title + cover.
type PostListItem struct {
	ID        int64           `json:"id"`
	Title     string          `json:"title"`
	SceneTag  int8            `json:"scene_tag"`
	SceneName string          `json:"scene_name"`
	CoverURL  string          `json:"cover_url"`
	LikeCount uint32          `json:"like_count"`
	ViewCount uint32          `json:"view_count"`
	Author    PostAuthorBrief `json:"author"`
	CreatedAt int64           `json:"created_at"` // UnixMilli
}

// PostDetailResp is the full detail view returned by GET /api/v1/posts/:id.
//
// Includes Content (vs. PostListItem which omits it) and audit state so
// authors viewing their own pending posts see "审核中" instead of an
// inexplicably-missing entry.
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
	AuditStatus uint8           `json:"audit_status"`
	IsVisible   uint8           `json:"is_visible"`
	CreatedAt   int64           `json:"created_at"` // UnixMilli
	UpdatedAt   int64           `json:"updated_at"` // UnixMilli
}

// FeedResp wraps a cursor-paginated feed page.
//
// Cursor encoding: decimal string of the last item's created_at UnixMilli.
// To fetch the next page, pass it back unchanged as the `cursor` query param.
// Empty next_cursor (and has_more=false) signal end-of-feed.
type FeedResp struct {
	Posts      []PostListItem `json:"posts"`
	NextCursor string         `json:"next_cursor"` // "" when no more
	HasMore    bool           `json:"has_more"`
}
