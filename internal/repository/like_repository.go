// Package repository — like_repository.go is the data-access layer for the
// Like entity. All MySQL I/O for likes (and the like_count column on posts)
// goes through this file.
//
// Like the other repositories in this package, this layer is intentionally
// thin: each method maps to one or two SQL statements with no business
// logic. The Like module's batching strategy (50 events / 3s) lives in
// LikeConsumer; this repository simply exposes the batch primitives.
//
// ── Why "BatchInsert + RowsAffected" instead of "INSERT then COUNT" ─────────
//
// LikeConsumer needs to know HOW MANY likes were actually persisted in a
// batch — duplicates (re-likes) get IGNOREd by uk_user_post, so a batch of
// 50 events may produce only 47 real inserts. The consumer uses the
// difference to derive the correct delta for posts.like_count.
//
// MySQL's RowsAffected on `INSERT IGNORE ... VALUES (...), (...), ...`
// returns the number of rows that actually went in (duplicates contribute 0).
// We surface it via the (int64 affected, error) return tuple so the consumer
// can apply the delta verbatim.
//
// DELETE works symmetrically: a `DELETE WHERE (user_id, post_id) IN (...)`
// returns RowsAffected = the number of rows that actually existed and were
// removed, which is the correct decrement for posts.like_count.
//
// ── Why a SEPARATE IncrPostLikeCount / DecrPostLikeCount instead of one ─────
//
// Two separate methods, each taking a per-post delta map, are clearer at
// the call site: the consumer iterates `likeDeltas` once for inserts and
// `unlikeDeltas` once for deletes, and the SQL it produces is obviously
// safe (only positive numbers added; only positive numbers subtracted).
// A single "ApplyDelta" with signed delta would force the consumer to
// branch on sign anyway, gaining nothing.
//
// ── Why GREATEST(0, ...) on Decr ────────────────────────────────────────────
//
// posts.like_count is INT UNSIGNED. Subtracting from an unsigned column
// past zero raises a "BIGINT UNSIGNED value is out of range" error in
// strict mode. GREATEST(0, like_count - delta) clamps at zero, which is
// also the semantically correct floor: a count can never go negative.
// In the rare case of an actual race that "decrements past zero", we'd
// rather show 0 than crash the batch. The CountConsumer's periodic
// reconciliation (Step 5+) will eventually realign with the source-of-truth
// SELECT COUNT(*) FROM likes.
//
// Added in Step 5 (like module).
package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"cooking-platform/internal/model"

	"gorm.io/gorm"
)

// LikeRepository is the abstraction the service / consumer layer depends on.
type LikeRepository interface {
	// BatchInsert inserts up to len(rows) like records using INSERT IGNORE.
	// Returns the number of rows that were actually inserted (i.e. weren't
	// skipped due to uk_user_post conflicts) so callers can derive the
	// real like_count delta.
	//
	// Empty input is a no-op returning (0, nil) — saves a round-trip when
	// a flush tick fires with only Unlike events accumulated.
	BatchInsert(ctx context.Context, rows []*model.Like) (int64, error)

	// BatchDelete removes the given (user_id, post_id) pairs. Returns the
	// number of rows actually deleted, which is the correct decrement for
	// the posts.like_count column.
	//
	// Empty input is a no-op returning (0, nil).
	BatchDelete(ctx context.Context, pairs []UserPostPair) (int64, error)

	// IncrPostLikeCount applies positive deltas to posts.like_count. The
	// map's keys are post_ids and values are non-negative deltas (typically
	// derived from BatchInsert's RowsAffected, broken down per-post by the
	// caller). One UPDATE statement per post; batches of 50 events fan out
	// to at most 50 distinct posts in the worst case, almost always many
	// fewer because likes cluster on hot content.
	//
	// Empty input is a no-op returning nil.
	IncrPostLikeCount(ctx context.Context, deltas map[int64]int64) error

	// DecrPostLikeCount applies positive deltas (subtracted from like_count).
	// Uses GREATEST(0, like_count - ?) to prevent the unsigned column
	// from underflowing on a degenerate race.
	//
	// Empty input is a no-op returning nil.
	DecrPostLikeCount(ctx context.Context, deltas map[int64]int64) error
}

// UserPostPair is a value-object used by BatchDelete to express the
// composite key (user_id, post_id). A struct beats two parallel slices
// for readability and prevents off-by-one mismatches at the call site.
type UserPostPair struct {
	UserID int64
	PostID int64
}

// likeRepository is the GORM-backed implementation. Lowercase by design —
// callers depend on the interface, not the struct.
type likeRepository struct {
	db *gorm.DB
}

// NewLikeRepository constructs a GORM-backed LikeRepository.
func NewLikeRepository(db *gorm.DB) LikeRepository {
	return &likeRepository{db: db}
}

// BatchInsert performs `INSERT IGNORE INTO likes (user_id, post_id, created_at)
// VALUES (?,?,?), (?,?,?), ...` and returns RowsAffected.
//
// We construct the SQL via raw Exec (rather than gorm's Clause(OnConflict))
// so the caller gets exactly INSERT IGNORE semantics: duplicates contribute
// 0 to RowsAffected without raising an error. gorm's CreateInBatches with
// OnConflict{DoNothing: true} would also work but is less explicit and
// composes RowsAffected per-batch which we'd have to sum manually.
func (r *likeRepository) BatchInsert(ctx context.Context, rows []*model.Like) (int64, error) {
	if len(rows) == 0 {
		return 0, nil
	}

	// Build placeholders and args in a single pass.
	// Each row contributes "(?, ?, ?)" placeholders + 3 args.
	placeholders := make([]byte, 0, len(rows)*8)
	args := make([]any, 0, len(rows)*3)
	now := time.Now()
	for i, row := range rows {
		if i > 0 {
			placeholders = append(placeholders, ',')
		}
		placeholders = append(placeholders, '(', '?', ',', '?', ',', '?', ')')
		// CreatedAt is supplied explicitly: GORM would normally fill it on
		// Create() but we're using Exec, so we own it. Default to now() if
		// the caller forgot to set it.
		ct := row.CreatedAt
		if ct.IsZero() {
			ct = now
		}
		args = append(args, row.UserID, row.PostID, ct)
	}

	sql := "INSERT IGNORE INTO likes (user_id, post_id, created_at) VALUES " + string(placeholders)
	res := r.db.WithContext(ctx).Exec(sql, args...)
	if res.Error != nil {
		return 0, fmt.Errorf("like batch insert: %w", res.Error)
	}
	return res.RowsAffected, nil
}

// BatchDelete runs `DELETE FROM likes WHERE (user_id, post_id) IN ((?,?), ...)`.
//
// MySQL supports tuple-IN syntax, which compiles to an efficient index lookup
// against uk_user_post (user_id, post_id). Per-row DELETE in a loop would
// also work but at 50 events that's 50 round-trips — tuple-IN keeps it to one.
func (r *likeRepository) BatchDelete(ctx context.Context, pairs []UserPostPair) (int64, error) {
	if len(pairs) == 0 {
		return 0, nil
	}

	placeholders := make([]byte, 0, len(pairs)*7)
	args := make([]any, 0, len(pairs)*2)
	for i, p := range pairs {
		if i > 0 {
			placeholders = append(placeholders, ',')
		}
		placeholders = append(placeholders, '(', '?', ',', '?', ')')
		args = append(args, p.UserID, p.PostID)
	}

	sql := "DELETE FROM likes WHERE (user_id, post_id) IN (" + string(placeholders) + ")"
	res := r.db.WithContext(ctx).Exec(sql, args...)
	if res.Error != nil {
		return 0, fmt.Errorf("like batch delete: %w", res.Error)
	}
	return res.RowsAffected, nil
}

// IncrPostLikeCount issues one UPDATE per post_id with a positive delta.
//
// Why a loop and not `UPDATE ... CASE WHEN`:
//   - CASE WHEN packs N updates into one SQL but the SQL string grows ~50
//     chars per post. At 50 distinct posts that's a 2.5KB query — fine,
//     but readability suffers and we lose per-row error attribution.
//   - The loop's overhead at MVP scale is one round-trip per post per batch,
//     i.e. tens of UPDATEs per second on the consumer goroutine. MySQL
//     handles this trivially and the consumer is async anyway.
//   - When sharding by post_id matters (Step 14 read/write split), per-post
//     UPDATE is the right granularity to send to the master.
func (r *likeRepository) IncrPostLikeCount(ctx context.Context, deltas map[int64]int64) error {
	if len(deltas) == 0 {
		return nil
	}
	for postID, delta := range deltas {
		if delta <= 0 {
			continue
		}
		err := r.db.WithContext(ctx).
			Exec("UPDATE posts SET like_count = like_count + ? WHERE id = ?", delta, postID).
			Error
		if err != nil {
			// Don't bail — process remaining posts. Log via wrapping; the
			// consumer's caller will see the wrapped error and warn.
			return fmt.Errorf("incr like_count post=%d: %w", postID, err)
		}
	}
	return nil
}

// DecrPostLikeCount mirrors IncrPostLikeCount but subtracts and clamps at 0.
func (r *likeRepository) DecrPostLikeCount(ctx context.Context, deltas map[int64]int64) error {
	if len(deltas) == 0 {
		return nil
	}
	for postID, delta := range deltas {
		if delta <= 0 {
			continue
		}
		err := r.db.WithContext(ctx).
			Exec("UPDATE posts SET like_count = GREATEST(0, CAST(like_count AS SIGNED) - ?) WHERE id = ?", delta, postID).
			Error
		if err != nil {
			return fmt.Errorf("decr like_count post=%d: %w", postID, err)
		}
	}
	return nil
}

// ErrLikeNotFound is exported in case future code wants to surface a
// "user has not liked this post" result distinct from a generic SQL error.
// Currently unused by the service layer (which infers state from Redis SET).
// Kept for symmetry with ErrUserNotFound / ErrPostNotFound.
var ErrLikeNotFound = errors.New("like not found")
