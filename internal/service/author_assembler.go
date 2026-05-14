// Package service — author_assembler.go is the single source of truth for
// turning model.Post rows into wire DTOs with author snapshots embedded.
//
// ── Why this file exists ───────────────────────────────────────────────────
//
// Before Step 7, the "load authors, embed author brief, build PostListItem"
// logic lived as private methods on *PostService (loadAuthor, assembleFeed,
// toListItem, makeAuthorBrief, uniqueAuthorIDs). Step 7's search module needs
// the exact same logic: a search result is also a list of posts with author
// snapshots. Copy-pasting ~30 lines into search_service.go would have left
// two divergent copies of a non-trivial invariant (deleted-user placeholder
// text, N+1 author loading, first-appearance-order dedup).
//
// So the logic is extracted here as a small reusable unit. post_service.go
// is refactored in the same step to consume it, so there is exactly ONE
// implementation — no drift between the feed path and the search path.
//
// ── Design ─────────────────────────────────────────────────────────────────
//
// AuthorAssembler holds the userRepo dependency (the part that needs I/O).
// The pure transformations (BuildAuthorBrief, BuildListItem) are package-level
// functions with no receiver — they are deterministic and trivially testable
// without a repo. PostService still owns toDetailResp (detail view) itself,
// because that shape is post-detail-specific and not shared with search.
//
// Added in Step 7 (search module).
package service

import (
	"context"
	"errors"
	"fmt"

	"cooking-platform/internal/model"
	"cooking-platform/internal/model/dto"
	"cooking-platform/internal/repository"
)

// AuthorAssembler loads author snapshots and assembles post list DTOs.
// Shared by PostService (feed / author-page) and SearchService (search).
type AuthorAssembler struct {
	userRepo repository.UserRepository
}

// NewAuthorAssembler constructs an AuthorAssembler. The userRepo is the
// same instance shared across services — assembler holds no state of its
// own, so a single instance is safe for concurrent use.
func NewAuthorAssembler(userRepo repository.UserRepository) *AuthorAssembler {
	return &AuthorAssembler{userRepo: userRepo}
}

// LoadOne returns a single post's author, or (nil, nil) if the author was
// soft-deleted. A nil user is a valid, expected result — callers render it
// via BuildAuthorBrief's deleted-user placeholder. A non-nil error means a
// real DB failure.
//
// Returning a placeholder rather than 404-ing the post matches PRD design:
// content survives author deletion so existing readers keep their context.
func (a *AuthorAssembler) LoadOne(ctx context.Context, userID int64) (*model.User, error) {
	u, err := a.userRepo.FindByID(ctx, userID)
	if err != nil {
		if errors.Is(err, repository.ErrUserNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("load author: %w", err)
	}
	return u, nil
}

// LoadMap loads every author referenced by posts into a lookup map keyed by
// user_id. Deleted authors are stored as nil values (the key is still
// present) so BuildListItem can distinguish "deleted user" from "not loaded".
//
// This performs N+1 lookups over the UNIQUE author ids — acceptable for
// page sizes <= 50 at ~1ms per query, consistent with the documented
// trade-off in post_service.go. When a 100+-post view appears, add
// userRepo.FindByIDs and swap it in here; every caller benefits at once.
func (a *AuthorAssembler) LoadMap(ctx context.Context, posts []*model.Post) (map[int64]*model.User, error) {
	authorMap := make(map[int64]*model.User, len(posts))
	for _, uid := range uniqueAuthorIDs(posts) {
		u, err := a.LoadOne(ctx, uid)
		if err != nil {
			return nil, err
		}
		// nil u (deleted user) is stored deliberately — see doc above.
		authorMap[uid] = u
	}
	return authorMap, nil
}

// BuildListItems converts a slice of posts into wire DTOs, looking each
// author up in the provided map. The map is expected to come from LoadMap
// over the same slice; a missing key is treated the same as a nil value
// (deleted-user placeholder).
func BuildListItems(posts []*model.Post, authorMap map[int64]*model.User) []dto.PostListItem {
	items := make([]dto.PostListItem, 0, len(posts))
	for _, p := range posts {
		items = append(items, BuildListItem(p, authorMap[p.UserID]))
	}
	return items
}

// BuildListItem converts one post + its author into the wire DTO. A nil
// author is rendered as the "deleted user" placeholder.
func BuildListItem(p *model.Post, author *model.User) dto.PostListItem {
	return dto.PostListItem{
		ID:        p.ID,
		Title:     p.Title,
		SceneTag:  int8(p.SceneTag),
		SceneName: p.SceneTag.Name(),
		CoverURL:  p.CoverURL,
		LikeCount: p.LikeCount,
		ViewCount: p.ViewCount,
		Author:    BuildAuthorBrief(p.UserID, author),
		CreatedAt: p.CreatedAt.UnixMilli(),
	}
}

// BuildAuthorBrief renders the embedded author DTO. A nil user yields the
// "用户已注销" placeholder carrying just the original author id, so the
// frontend can still show *something* without a broken link.
func BuildAuthorBrief(authorID int64, u *model.User) dto.PostAuthorBrief {
	if u != nil {
		return dto.PostAuthorBrief{
			ID:        u.ID,
			Nickname:  u.Nickname,
			AvatarURL: u.AvatarURL,
		}
	}
	return dto.PostAuthorBrief{
		ID:       authorID,
		Nickname: "（用户已注销）",
	}
}

// uniqueAuthorIDs preserves first-appearance order while deduplicating.
// Order preservation is a courtesy for log readability — set semantics
// would suffice for correctness. Moved here from post_service.go in Step 7
// so both PostService and SearchService share the one implementation.
func uniqueAuthorIDs(posts []*model.Post) []int64 {
	seen := make(map[int64]struct{}, len(posts))
	out := make([]int64, 0, len(posts))
	for _, p := range posts {
		if _, ok := seen[p.UserID]; !ok {
			seen[p.UserID] = struct{}{}
			out = append(out, p.UserID)
		}
	}
	return out
}
