// Package repository — follow_repository.go is the data-access layer for the
// Follow entity. All MySQL I/O for the follows table goes through this file.
//
// Like the other repositories in this package, this layer is intentionally
// thin: each method maps to one or two SQL statements with no business
// logic. The "can't follow self / 3000-follow cap / idempotent re-follow"
// rules all live in the service layer (follow_service.go).
//
// ── Why raw INSERT IGNORE / DELETE + RowsAffected ──────────────────────────
//
// Create and Delete return a bool ("did a row actually change?") rather than
// just an error. The service layer needs that bool to decide whether to
// publish a FollowEvent / UnfollowEvent:
//
//   - Create uses INSERT IGNORE: a duplicate (re-following someone you
//     already follow) is absorbed by uk_follower_following → RowsAffected=0.
//     The service sees inserted=false and skips the event — no double count.
//   - Delete returns RowsAffected>0 only when a row really existed. The
//     service maps deleted=false to ErrFollowNotFound (440103) per the
//     Step 8 contract ("取消未关注的人").
//
// This mirrors like_repository.go's BatchInsert/BatchDelete RowsAffected
// pattern, scaled down to single-row operations — follow is low-frequency,
// so there is no batching (see Step 8 故事线 / ADR).
//
// ── Why ListFollowers / ListFollowing JOIN users ───────────────────────────
//
// The follower / following lists render "头像 + 昵称" per row. Returning bare
// follow edges would force the service into an N+1 (one FindByID per edge).
// A single `follows JOIN users` query returns the user fields directly, and
// also carries follows.id as follow_id — the keyset cursor value — so the
// service paginates without a second query.
//
// Soft-deleted users (注销账号) are excluded via `users.deleted_at IS NULL`
// in the JOIN. A deactivated user simply drops out of follow lists, which
// matches PRD-Phase2 §8 F-F02 ("关注的人全部注销 → 空状态"). No placeholder
// row is needed, unlike the post-author case where content survives its
// author — that asymmetry is deliberate.
//
// ── Keyset pagination on follows.id ────────────────────────────────────────
//
// follows.id (BIGINT UNSIGNED AUTO_INCREMENT) is monotonic, stable and
// unique — an ideal keyset cursor, free of the same-millisecond tie risk a
// created_at cursor carries. Pages are ordered `f.id DESC` (newest follow
// first); cursorFollowID=0 means "first page".
//
//   - ListFollowers: `WHERE following_id=? AND f.id<?` is index-efficient —
//     idx_following_id is implicitly (following_id, id) in InnoDB.
//   - ListFollowing: `WHERE follower_id=? AND f.id<?` uses uk_follower_
//     following's follower_id prefix; the f.id range is not perfectly
//     index-covered, but the 3000-follow cap bounds the scan to ≤3000 rows.
//     Acceptable at MVP scale; revisit with a dedicated index only if a
//     slow-query alert ever fires.
//
// Added in Step 8 (follow module).
package repository

import (
	"context"
	"errors"
	"fmt"

	"cooking-platform/internal/model"

	"gorm.io/gorm"
)

// ErrFollowNotFound is the sentinel for "unfollow targeted a relationship
// that does not exist". The repository does not return it directly (Delete
// reports absence via deleted=false); it is exported here for symmetry with
// ErrUserNotFound / ErrPostNotFound so the service references one canonical
// value when mapping to errcode.ErrFollowNotFound (440103).
var ErrFollowNotFound = errors.New("follow relationship not found")

// FollowUser is one row of a follower / following list: the fields the list
// renders (id / nickname / avatar) plus follows.id of the edge — the keyset
// cursor value.
//
// It deliberately does NOT embed model.User. model.User carries a
// gorm.DeletedAt field; embedding it into a struct scanned via
// .Table("follows AS f") would make GORM's soft-delete clause try to append
// `deleted_at IS NULL` against the follows table (which has no such column)
// → broken SQL. A flat struct with only the needed columns sidesteps that.
// The gorm column tags let GORM scan the JOIN result straight into it.
type FollowUser struct {
	UserID    int64  `gorm:"column:id"`
	Nickname  string `gorm:"column:nickname"`
	AvatarURL string `gorm:"column:avatar_url"`
	FollowID  int64  `gorm:"column:follow_id"`
}

// FollowRepository is the abstraction the service layer depends on.
// Callers never see *gorm.DB — same boundary discipline as the other repos.
type FollowRepository interface {
	// Create inserts a follow edge using INSERT IGNORE. Returns inserted=true
	// when a new row was written, inserted=false when the edge already
	// existed (uk_follower_following conflict, absorbed silently). The
	// service uses the bool to decide whether to publish a FollowEvent.
	Create(ctx context.Context, followerID, followingID int64) (inserted bool, err error)

	// Delete removes a follow edge. Returns deleted=true when a row was
	// actually removed, deleted=false when no such edge existed. The service
	// maps deleted=false to ErrFollowNotFound (440103).
	Delete(ctx context.Context, followerID, followingID int64) (deleted bool, err error)

	// Exists reports whether followerID currently follows followingID. Used
	// by the service for the idempotent re-follow fast path: return
	// "following=true" without the 3000-cap check or an event.
	Exists(ctx context.Context, followerID, followingID int64) (bool, error)

	// CountFollowing returns how many users followerID currently follows.
	// Used to enforce the 3000-follow cap (AC-5). Counted from the follows
	// table directly — NOT users.following_count — because the redundant
	// counter is eventually-consistent (CountConsumer batches 20/10s) and a
	// hard cap must not be enforced against a stale number.
	CountFollowing(ctx context.Context, followerID int64) (int64, error)

	// ListFollowers returns the users who follow followingID, newest follow
	// first, keyset-paginated on follows.id. cursorFollowID=0 → first page;
	// otherwise rows with follows.id < cursorFollowID. Callers pass size+1 to
	// detect has_more without a COUNT query. Soft-deleted users are excluded.
	ListFollowers(ctx context.Context, followingID, cursorFollowID int64, size int) ([]FollowUser, error)

	// ListFollowing returns the users that followerID follows, newest follow
	// first, keyset-paginated on follows.id. Same cursor / size+1 contract
	// as ListFollowers.
	ListFollowing(ctx context.Context, followerID, cursorFollowID int64, size int) ([]FollowUser, error)
}

// followRepository is the GORM-backed implementation. Lowercase by design —
// callers depend on the interface, not the struct.
type followRepository struct {
	db *gorm.DB
}

// NewFollowRepository constructs a GORM-backed FollowRepository.
func NewFollowRepository(db *gorm.DB) FollowRepository {
	return &followRepository{db: db}
}

// Create runs `INSERT IGNORE INTO follows (follower_id, following_id) VALUES (?, ?)`.
//
// created_at is left to the column's DEFAULT CURRENT_TIMESTAMP(3) — unlike
// like_repository.BatchInsert (which sets created_at explicitly because it
// builds a multi-row VALUES list), a single-row insert can rely on the DB
// default cleanly.
//
// INSERT IGNORE means a duplicate edge contributes 0 to RowsAffected without
// raising an error — exactly the idempotency semantics the service wants.
func (r *followRepository) Create(ctx context.Context, followerID, followingID int64) (bool, error) {
	res := r.db.WithContext(ctx).Exec(
		"INSERT IGNORE INTO follows (follower_id, following_id) VALUES (?, ?)",
		followerID, followingID,
	)
	if res.Error != nil {
		return false, fmt.Errorf("follow create: %w", res.Error)
	}
	return res.RowsAffected > 0, nil
}

// Delete runs `DELETE FROM follows WHERE follower_id = ? AND following_id = ?`.
//
// The (follower_id, following_id) predicate is a direct unique-index lookup
// on uk_follower_following — at most one row matches. RowsAffected is 1 when
// the edge existed, 0 when it didn't.
func (r *followRepository) Delete(ctx context.Context, followerID, followingID int64) (bool, error) {
	res := r.db.WithContext(ctx).Exec(
		"DELETE FROM follows WHERE follower_id = ? AND following_id = ?",
		followerID, followingID,
	)
	if res.Error != nil {
		return false, fmt.Errorf("follow delete: %w", res.Error)
	}
	return res.RowsAffected > 0, nil
}

// Exists probes uk_follower_following for the edge. `SELECT EXISTS(...)`
// always returns exactly one row carrying 0 or 1 — cleaner to scan than a
// nullable `SELECT 1` that returns zero rows on a miss.
func (r *followRepository) Exists(ctx context.Context, followerID, followingID int64) (bool, error) {
	var exists bool
	err := r.db.WithContext(ctx).
		Raw("SELECT EXISTS(SELECT 1 FROM follows WHERE follower_id = ? AND following_id = ?)",
			followerID, followingID).
		Scan(&exists).Error
	if err != nil {
		return false, fmt.Errorf("follow exists: %w", err)
	}
	return exists, nil
}

// CountFollowing runs `SELECT count(*) FROM follows WHERE follower_id = ?`.
//
// Counted from the source-of-truth table, not users.following_count: the
// redundant counter lags behind, and a hard cap must be enforced against the
// authoritative number.
func (r *followRepository) CountFollowing(ctx context.Context, followerID int64) (int64, error) {
	var cnt int64
	err := r.db.WithContext(ctx).
		Model(&model.Follow{}).
		Where("follower_id = ?", followerID).
		Count(&cnt).Error
	if err != nil {
		return 0, fmt.Errorf("follow count following: %w", err)
	}
	return cnt, nil
}

// ListFollowers — see interface doc. Pivots the JOIN on f.follower_id and
// filters by f.following_id.
func (r *followRepository) ListFollowers(ctx context.Context, followingID, cursorFollowID int64, size int) ([]FollowUser, error) {
	return r.listJoin(ctx, joinDirFollowers, followingID, cursorFollowID, size)
}

// ListFollowing — see interface doc. Same query as ListFollowers with the
// two follows columns swapped: pivots on f.following_id, filters by
// f.follower_id.
func (r *followRepository) ListFollowing(ctx context.Context, followerID, cursorFollowID int64, size int) ([]FollowUser, error) {
	return r.listJoin(ctx, joinDirFollowing, followerID, cursorFollowID, size)
}

// joinDir selects which side of the follows edge the list query pivots on.
// followers and following lists are the exact same query with two columns
// swapped — a typed direction beats copy-pasting the whole method body.
type joinDir int8

const (
	joinDirFollowers joinDir = iota // users who follow :id  → JOIN on follower_id, WHERE following_id
	joinDirFollowing                // users :id follows     → JOIN on following_id, WHERE follower_id
)

// listJoin is the shared implementation behind ListFollowers / ListFollowing.
//
// Query shape (ListFollowers direction shown):
//
//	SELECT u.id, u.nickname, u.avatar_url, f.id AS follow_id
//	FROM follows AS f
//	JOIN users u ON u.id = f.follower_id
//	WHERE f.following_id = ?
//	  AND u.deleted_at IS NULL
//	  [AND f.id < ?]
//	ORDER BY f.id DESC
//	LIMIT ?
//
// joinCol / whereCol are internal constants (never user input), so building
// the JOIN/WHERE fragments by string concatenation carries no injection risk.
func (r *followRepository) listJoin(ctx context.Context, dir joinDir, pivotID, cursorFollowID int64, size int) ([]FollowUser, error) {
	var joinCol, whereCol string
	switch dir {
	case joinDirFollowers:
		joinCol, whereCol = "f.follower_id", "f.following_id"
	case joinDirFollowing:
		joinCol, whereCol = "f.following_id", "f.follower_id"
	default:
		return nil, fmt.Errorf("follow list join: unknown direction %d", dir)
	}

	q := r.db.WithContext(ctx).
		Table("follows AS f").
		Select("u.id, u.nickname, u.avatar_url, f.id AS follow_id").
		Joins("JOIN users u ON u.id = "+joinCol).
		Where(whereCol+" = ?", pivotID).
		Where("u.deleted_at IS NULL")

	if cursorFollowID > 0 {
		q = q.Where("f.id < ?", cursorFollowID)
	}

	var rows []FollowUser
	if err := q.Order("f.id DESC").Limit(size).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("follow list join: %w", err)
	}
	return rows, nil
}
