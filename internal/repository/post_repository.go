// Package repository — post_repository.go is the data-access layer for the
// Post entity. All MySQL I/O for posts goes through this file.
//
// Like user_repository.go, this layer is intentionally thin: each method
// maps to one or two SQL statements with no business logic. Cursor-paginated
// reads are the central concern — see ListFeed / ListByUser comments below.
//
// Why cursor pagination, not offset:
//   - LIMIT 100000, 20 forces MySQL to read & discard 100,000 rows. Latency
//     grows linearly with the page number — terrible UX for engaged users
//     who scroll for minutes.
//   - WHERE created_at < ? LIMIT 20 only reads 20 rows from the index leaf
//     starting at the cursor's position. Latency is constant regardless of
//     how deep the user has scrolled.
//   - Cursor pagination also dodges duplicate/skip bugs when new rows are
//     inserted between page fetches (offset shifts; cursor doesn't).
//
// Why no FOR UPDATE / explicit transactions in this file:
//   - Step 4's writes are single-row INSERTs (Create), single-row UPDATEs
//     (later steps), and pure reads. None requires multi-row atomicity.
//   - When AuditConsumer (Step 10) needs to flip is_visible + write
//     audit_log atomically, it will pass *gorm.DB explicitly to a new
//     method (e.g. UpdateAuditStatusTx). We add that signature only when
//     the consumer lands; YAGNI for now.
//
// Future improvements:
//   - Read-replica routing: when DBResolver lands (Step 14), reads use the
//     slave session and writes the master. The split happens transparently
//     to this file via gorm.DB middleware — no method-level changes needed.
//   - Index hint: if EXPLAIN shows MySQL picking the wrong index for some
//     scene_tag combinations, add `USE INDEX(idx_scene_visible_created)`
//     hints. Premature today; revisit at the first slow-query alert.
//   - Batch loading: feed cards need authors; today the service makes N+1
//     queries (one per unique author). Acceptable for size<=20; if a
//     "follow feed" view ever pulls 100+ posts, add FindByIDs(ids).
//
// Added in Step 4 (content module).
package repository

import (
	"context"
	"errors"
	"time"

	"cooking-platform/internal/model"

	"gorm.io/gorm"
)

// ErrPostNotFound is returned when a single-row query expects exactly one
// post but none is found. Service layer maps it to errcode.ErrPostNotFound
// for the HTTP response.
var ErrPostNotFound = errors.New("post not found")

// PostRepository is the abstraction the service layer depends on.
// Caller never sees *gorm.DB — that boundary is intentional, see file header.
type PostRepository interface {
	// Create inserts a new post. The post's ID is populated on success.
	Create(ctx context.Context, p *model.Post) error

	// FindByID looks up a post by primary key.
	// Returns ErrPostNotFound if no row matches; soft-deleted rows are
	// excluded automatically by GORM via DeletedAt.
	FindByID(ctx context.Context, id int64) (*model.Post, error)

	// ListFeed returns visible posts ordered by created_at DESC, optionally
	// filtered by scene tag. Strictly older than `cursorTime` (exclusive).
	//
	//   scene == 0  → no scene filter (homepage feed)
	//   scene != 0  → scene-filtered feed
	//
	// When cursorTime.IsZero(), no cursor predicate is applied → first page.
	// size is clamped to [1, 50] by the service layer; this method trusts it.
	ListFeed(ctx context.Context, scene int8, cursorTime time.Time, size int) ([]*model.Post, error)

	// ListByUser is the author-page feed: all posts by a given user,
	// regardless of is_visible (authors see their own pending/rejected posts).
	//
	// MVP note: the "regardless of is_visible" rule is correct only when the
	// caller already verified ownership. The service layer enforces this:
	// strangers viewing :id get is_visible=1 only; the author viewing their
	// own page sees everything. We keep the repository neutral so the
	// service can choose either policy with a single boolean.
	ListByUser(ctx context.Context, userID int64, includeInvisible bool, cursorTime time.Time, size int) ([]*model.Post, error)
}

// postRepository is the GORM-backed implementation. Lowercase by design —
// callers depend on the interface, not the struct.
type postRepository struct {
	db *gorm.DB
}

// NewPostRepository constructs a GORM-backed PostRepository.
func NewPostRepository(db *gorm.DB) PostRepository {
	return &postRepository{db: db}
}

// Create inserts a new post row. GORM populates p.ID from LAST_INSERT_ID
// on success. We do NOT validate fields here — that's the service layer's
// job; the repository trusts whatever it's handed.
func (r *postRepository) Create(ctx context.Context, p *model.Post) error {
	return r.db.WithContext(ctx).Create(p).Error
}

// FindByID looks up a post by primary key.
func (r *postRepository) FindByID(ctx context.Context, id int64) (*model.Post, error) {
	var p model.Post
	err := r.db.WithContext(ctx).First(&p, id).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrPostNotFound
		}
		return nil, err
	}
	return &p, nil
}

// ListFeed runs the homepage / scene-filtered feed query.
//
// Index targeting (verified via EXPLAIN at design time):
//   - scene == 0 → idx_visible_created (is_visible, created_at DESC)
//   - scene != 0 → idx_scene_visible_created (scene_tag, is_visible, created_at DESC)
//
// The query builder constructs WHERE clauses in a fixed order so the
// optimiser sees the same predicate shape every call → consistent plan
// caching → predictable latency.
func (r *postRepository) ListFeed(ctx context.Context, scene int8, cursorTime time.Time, size int) ([]*model.Post, error) {
	q := r.db.WithContext(ctx).
		Model(&model.Post{}).
		Where("is_visible = ?", model.PostVisible)

	if scene != 0 {
		q = q.Where("scene_tag = ?", scene)
	}
	if !cursorTime.IsZero() {
		// Strictly less-than: callers pass the previous page's last
		// created_at, and we don't want to repeat it. Equality ties (same
		// millisecond) are extremely rare with DATETIME(3); when they
		// happen the next page may skip one row, which is acceptable
		// (Feed is best-effort — users can pull-to-refresh). True
		// dedup would require a (created_at, id) tuple cursor — overkill
		// for MVP volume.
		q = q.Where("created_at < ?", cursorTime)
	}

	var posts []*model.Post
	err := q.Order("created_at DESC").
		Limit(size).
		Find(&posts).Error
	if err != nil {
		return nil, err
	}
	return posts, nil
}

// ListByUser runs the author-page feed query.
//
// includeInvisible controls whether invisible (audit-pending / rejected)
// posts are returned. The service layer sets it true only for the author
// viewing their own page.
//
// Index target: idx_user_created (user_id, created_at DESC). Both branches
// of the includeInvisible if-statement keep user_id as the leftmost
// predicate, so the index is always usable.
func (r *postRepository) ListByUser(ctx context.Context, userID int64, includeInvisible bool, cursorTime time.Time, size int) ([]*model.Post, error) {
	q := r.db.WithContext(ctx).
		Model(&model.Post{}).
		Where("user_id = ?", userID)

	if !includeInvisible {
		q = q.Where("is_visible = ?", model.PostVisible)
	}
	if !cursorTime.IsZero() {
		q = q.Where("created_at < ?", cursorTime)
	}

	var posts []*model.Post
	err := q.Order("created_at DESC").
		Limit(size).
		Find(&posts).Error
	if err != nil {
		return nil, err
	}
	return posts, nil
}
