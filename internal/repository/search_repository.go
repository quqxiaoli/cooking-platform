// Package repository — search_repository.go is the data-access layer for
// full-text content search. It is the only file that issues MySQL FULLTEXT
// (MATCH ... AGAINST) queries.
//
// Like the other repositories in this package, it is intentionally thin:
// one method, one SQL statement, zero business logic. Keyword sanitisation,
// length clamping, cursor<->offset translation, and author-snapshot
// assembly all live in the service layer (search_service.go).
//
// ── Why a separate SearchRepository, not a method on PostRepository ────────
//
// PostRepository's reads (ListFeed / ListByUser) are all keyset-paginated on
// `created_at` — a deliberate, documented contract (see post_repository.go
// header). Full-text search cannot honour that contract: results are ranked
// by FULLTEXT relevance score, which is not a monotonic, stable column you
// can keyset-paginate on. Bolting a relevance-ranked, offset-paginated query
// onto PostRepository would muddy that file's "everything is cursor-based"
// invariant. A dedicated repository keeps each file's pagination story
// internally consistent.
//
// ── Why offset pagination (a deliberate deviation from PRD-Phase3 §5.4) ────
//
// §5.4 mandates keyset cursor pagination ("query size+1 rows") for all list
// endpoints. Search is the documented exception:
//
//   - The sort key is FULLTEXT relevance, recomputed per query. It is not a
//     stored column, has ties, and shifts as posts are inserted/edited —
//     none of the properties a keyset cursor needs.
//   - A (relevance, id) tuple cursor is unsafe: the same post can score
//     differently between two requests, causing rows to be skipped or
//     repeated across pages.
//   - Search is a "first 1-3 pages" access pattern in practice. Offset's
//     deep-page cost (LIMIT 100000, 20) is a non-issue here, unlike the
//     infinite-scroll Feed.
//
// So search uses classic OFFSET/LIMIT. The service layer still hides this
// behind an opaque `cursor` string (carrying the decimal offset) so the
// wire API shape matches §7.1's `?cursor=` parameter — clients never learn
// it is really an offset, leaving room to migrate later.
//
// ── Index behaviour ────────────────────────────────────────────────────────
//
// The query is `WHERE MATCH(title) AGAINST(? IN BOOLEAN MODE) AND
// is_visible = 1 [AND scene_tag = ?]`. MySQL drives the query through the
// `ft_title` FULLTEXT index for the MATCH predicate, then applies
// is_visible / scene_tag as post-filters on the (small) candidate set.
// For ~100k posts and a reasonably selective keyword this stays well under
// the AC-1 budget (P95 < 500ms). FULLTEXT and a secondary B-tree index
// cannot both drive the same query — that is expected and fine at MVP scale;
// if a slow-query alert ever fires, the fix is a generated-column +
// composite FULLTEXT index, revisited then (YAGNI now).
//
// Added in Step 7 (search module).
package repository

import (
	"context"

	"cooking-platform/internal/model"

	"gorm.io/gorm"
)

// SearchRepository is the abstraction the search service depends on.
// Callers never see *gorm.DB — same boundary discipline as the other repos.
type SearchRepository interface {
	// SearchByTitle runs a MySQL FULLTEXT search over posts.title using the
	// ngram parser in BOOLEAN MODE, returning only visible posts.
	//
	//   keyword — already trimmed, length-clamped, and BOOLEAN-MODE-sanitised
	//             by the service layer. The repository trusts it verbatim
	//             (consistent with post_repository.Create trusting its input).
	//   scene   — 0 means "no scene filter"; 1..8 filters to that scene tag.
	//   offset  — rows to skip (service translates the opaque cursor into this).
	//   size    — max rows to return. The service passes limit+1 so it can
	//             detect has_more without a separate COUNT query.
	//
	// Ordering: FULLTEXT relevance DESC, then like_count DESC (PRD §7 F-S01).
	// An empty result set is returned as an empty slice with a nil error —
	// "no matches" is a normal outcome, not a failure.
	SearchByTitle(ctx context.Context, keyword string, scene int8, offset, size int) ([]*model.Post, error)
}

// searchRepository is the GORM-backed implementation of SearchRepository.
// Lowercase by design — callers depend on the interface, not the struct.
type searchRepository struct {
	db *gorm.DB
}

// NewSearchRepository constructs a GORM-backed SearchRepository.
func NewSearchRepository(db *gorm.DB) SearchRepository {
	return &searchRepository{db: db}
}

// matchExpr is the FULLTEXT predicate, declared once so the SELECT alias and
// the WHERE clause use a byte-identical expression. MySQL recognises the
// repeated MATCH(...) AGAINST(...) and computes the relevance score a single
// time per row, then reuses it for both filtering and ranking.
const matchExpr = "MATCH(`title`) AGAINST(? IN BOOLEAN MODE)"

// SearchByTitle implements SearchRepository.
//
// Query shape:
//
//	SELECT `posts`.*, MATCH(`title`) AGAINST(? IN BOOLEAN MODE) AS relevance
//	FROM `posts`
//	WHERE `posts`.`deleted_at` IS NULL          -- GORM soft-delete, automatic
//	  AND is_visible = 1
//	  AND MATCH(`title`) AGAINST(? IN BOOLEAN MODE)
//	  [AND scene_tag = ?]
//	ORDER BY relevance DESC, like_count DESC
//	LIMIT ? OFFSET ?
//
// `relevance` is selected purely to drive ORDER BY. model.Post has no
// Relevance field, so GORM silently drops the extra column on scan — the
// search response is assembled from the normal post columns only.
func (r *searchRepository) SearchByTitle(ctx context.Context, keyword string, scene int8, offset, size int) ([]*model.Post, error) {
	q := r.db.WithContext(ctx).
		Model(&model.Post{}).
		Select("`posts`.*, "+matchExpr+" AS relevance", keyword).
		Where("is_visible = ?", model.PostVisible).
		Where(matchExpr, keyword)

	// Scene filter is optional. Keeping it as a trailing equality predicate
	// (not folded into the MATCH) means the FULLTEXT index still drives the
	// query; scene_tag is applied to the candidate rows afterwards.
	if scene != 0 {
		q = q.Where("scene_tag = ?", scene)
	}

	var posts []*model.Post
	err := q.
		Order("relevance DESC").
		Order("like_count DESC").
		Offset(offset).
		Limit(size).
		Find(&posts).Error
	if err != nil {
		return nil, err
	}
	return posts, nil
}
