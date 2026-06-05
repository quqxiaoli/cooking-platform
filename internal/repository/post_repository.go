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
// Why no FOR UPDATE / explicit transactions on Create / FindByID etc:
//   - Step 4's writes are single-row INSERTs (Create), single-row UPDATEs
//     (later steps), and pure reads. None requires multi-row atomicity.
//   - Step 9 added CreateWithSteps which DOES use an explicit transaction —
//     post + post_steps must land atomically so detail-page LoadSteps
//     never sees a half-published post. See that method's docs.
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
// Added in Step 4 (content module). Extended in Step 9 with
// CreateWithSteps + LoadSteps for the post_steps subtable.
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

	// CreateWithSteps inserts a post and its steps atomically in a single
	// transaction. p.ID and each step.ID/PostID are populated on success.
	//
	// Pass an empty `steps` slice to fall back to single-row Create
	// semantics — the transaction is then degenerate (one INSERT) and
	// incurs roughly the same cost as plain Create.
	//
	// Atomic semantics matter because the detail page LoadSteps after the
	// post becomes visible; a half-committed state would surface a
	// stepless ghost post to readers.
	//
	// Added in Step 9 alongside the post_steps subtable for structured
	// content (PRD-Phase2 §F-C01: 1..30 steps, 0..3 images each).
	CreateWithSteps(ctx context.Context, p *model.Post, steps []*model.PostStep) error

	// FindByID looks up a post by primary key.
	// Returns ErrPostNotFound if no row matches; soft-deleted rows are
	// excluded automatically by GORM via DeletedAt.
	FindByID(ctx context.Context, id int64) (*model.Post, error)

	// LoadSteps returns the steps for a given post ordered by step_no ASC.
	// Returns an empty slice (not an error) when the post has no steps —
	// callers must treat empty as "legacy text-only post" rather than
	// "missing data".
	//
	// Soft-deleted posts are NOT filtered here; the caller has already
	// retrieved the parent Post via FindByID, which applies the soft-delete
	// filter. Duplicating that check here would double the DB cost without
	// guarding against any realistic call site.
	//
	// Added in Step 9.
	LoadSteps(ctx context.Context, postID int64) ([]*model.PostStep, error)

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

	// UpdateAuditStatus sets audit_status, is_visible, and audit_remark on
	// a post in one UPDATE statement. Called only by AuditConsumer (Step 10)
	// after the content-safety API returns a verdict.
	//
	// Using a dedicated method (rather than a generic Update) keeps the
	// audit state transition explicit and auditable: grep for
	// UpdateAuditStatus to find every code path that changes visibility.
	UpdateAuditStatus(ctx context.Context, postID int64, auditStatus, isVisible uint8, remark string) error
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

// CreateWithSteps inserts the post and its steps in one transaction.
//
// Order matters:
//  1. INSERT posts → p.ID populated by GORM from LAST_INSERT_ID.
//  2. Stamp each step.PostID = p.ID (callers don't know it yet).
//  3. Batch INSERT post_steps in a single statement (GORM auto-batches
//     when given a slice).
//
// Empty `steps` is allowed and results in a single INSERT inside the
// transaction — the price of holding a transaction for one statement is
// negligible (single round-trip + auto-commit) and keeps the API uniform
// for the caller.
func (r *postRepository) CreateWithSteps(ctx context.Context, p *model.Post, steps []*model.PostStep) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(p).Error; err != nil {
			return err
		}
		if len(steps) == 0 {
			return nil
		}
		for _, s := range steps {
			s.PostID = p.ID
		}
		return tx.Create(&steps).Error
	})
}

// FindByID looks up a post by primary key.
func (r *postRepository) FindByID(ctx context.Context, id int64) (*model.Post, error) {
	var p model.Post
	err := Apply(ctx, r.db).First(&p, id).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrPostNotFound
		}
		return nil, err
	}
	return &p, nil
}

// LoadSteps returns the steps of a post ordered by step_no ASC.
//
// idx_post (post_id) is the target index — GORM compiles to:
//
//	SELECT ... FROM post_steps WHERE post_id = ? ORDER BY step_no ASC
//
// MySQL satisfies the WHERE with the index, then sorts the (small,
// up-to-30-row) result set in memory. Cheap.
func (r *postRepository) LoadSteps(ctx context.Context, postID int64) ([]*model.PostStep, error) {
	var steps []*model.PostStep
	err := Apply(ctx, r.db).
		Where("post_id = ?", postID).
		Order("step_no ASC").
		Find(&steps).Error
	if err != nil {
		return nil, err
	}
	return steps, nil
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

// UpdateAuditStatus executes a targeted UPDATE on the three audit columns.
//
// We use map[string]interface{} instead of a struct pointer so GORM does
// not skip zero-value fields (e.g. is_visible=0 when a post is rejected).
// The WHERE clause targets only one row (primary key), so no index hint
// is needed and the statement is always O(1).
func (r *postRepository) UpdateAuditStatus(ctx context.Context, postID int64, auditStatus, isVisible uint8, remark string) error {
	return r.db.WithContext(ctx).
		Model(&model.Post{}).
		Where("id = ?", postID).
		Updates(map[string]interface{}{
			"audit_status": auditStatus,
			"is_visible":   isVisible,
			"audit_remark": remark,
		}).Error
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
	q := Apply(ctx, r.db).
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
