// Package dto — search.go defines request and response DTOs for the search module.
//
// Consistent with post.go's DTO decisions:
//
//   - SceneTag is exposed as int8, not model.SceneTag — the dto package must
//     not import internal/model (avoids coupling wire format to domain types).
//
//   - Cursor is an opaque string. For search it carries a decimal OFFSET
//     rather than a timestamp, but clients MUST NOT parse it either way —
//     they pass back next_cursor verbatim. Keeping it opaque means search
//     can migrate from offset to a smarter scheme later without an API break.
//     See internal/repository/search_repository.go for why search uses
//     offset instead of the keyset cursor used by feed.
//
//   - SearchResp is a distinct type from FeedResp even though the fields
//     line up today. The two have different cursor semantics (offset vs
//     created_at) and will diverge — search may later carry a total count
//     or an echoed/normalised query string. A shared type would make those
//     additions leak across both endpoints.
//
// Added in Step 7 (search module).
package dto

// SearchQuery is the query-string struct for GET /api/v1/search.
//
// Field rules:
//
//   - q: the search keyword. Validation is intentionally NOT done via gin
//     binding here. PRD §7 F-S01 has two different rules for two cases:
//     empty/whitespace → reject (AC-2), and over-length → truncate (AC-7).
//     A binding tag can only reject, not truncate, so the service layer
//     (search_service.normaliseKeyword + the empty check) owns both rules.
//     Leaving q tag-free keeps that ownership in one place.
//
//   - scene_tag: optional scene filter, 1..8. Named scene_tag (not the
//     §7.1 draft's `scene`) to stay consistent with FeedQuery — the
//     frontend already speaks scene_tag everywhere else. 0/absent means
//     "all scenes". omitempty so an absent param doesn't fail binding.
//     Note PRD AC-5 says scene filtering is done client-side after one
//     search; this server-side param is the optional belt-and-braces path
//     and costs nothing to support.
//
//   - cursor: opaque pagination token (decimal offset). "" = first page.
//
//   - size: page size, 1..50, service defaults it to 20 when 0.
type SearchQuery struct {
	Keyword  string `form:"q"`
	SceneTag int8   `form:"scene_tag" binding:"omitempty,min=1,max=8"`
	Cursor   string `form:"cursor"`
	Size     int    `form:"size" binding:"omitempty,min=1,max=50"`
}

// SearchResp wraps one page of search results.
//
// Posts reuses dto.PostListItem — a search result card shows exactly the
// same fields as a feed card (title, cover, scene, counts, author). Content
// is excluded for the same reason as feed lists: result cards don't render
// the body.
//
// next_cursor + has_more mirror FeedResp's redundant-signal design: a
// brittle client checking `next_cursor != ""` still works alongside one
// that trusts has_more. Empty next_cursor (and has_more=false) = last page.
type SearchResp struct {
	Posts      []PostListItem `json:"posts"`
	NextCursor string         `json:"next_cursor"` // "" when no more
	HasMore    bool           `json:"has_more"`
}
