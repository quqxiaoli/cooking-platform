// Package model — follow.go defines the GORM model that maps 1-to-1 to the
// `follows` table created by migrations/000004_create_follows_table.up.sql.
//
// Like model.Like, this entity is deliberately minimal — 4 columns, and
// notably NO UpdatedAt and NO DeletedAt:
//
//   - No UpdatedAt: a follow row is immutable. It either exists (the follow
//     relationship is active) or it doesn't (unfollowed). No column ever
//     changes, so an UpdatedAt would be dead weight.
//
//   - No DeletedAt: unfollow is a physical DELETE, not a soft delete. Soft
//     delete would break the uk_follower_following unique constraint —
//     re-following after an unfollow would collide with the residual
//     soft-deleted row. See the migration file header for the full rationale.
//
// As with the other models, the SQL migration is the source of truth for
// column types and indices; the struct tags below are documentary.
//
// Added in Step 8 (follow module).
package model

import "time"

// Follow is the GORM-managed entity for the follows table.
//
// Read the pair as "FollowerID → FollowingID": FollowerID follows
// FollowingID. The pair is unique (uk_follower_following) — a user can
// follow another user at most once.
type Follow struct {
	ID          int64     `gorm:"primaryKey;column:id"`
	FollowerID  int64     `gorm:"column:follower_id;not null;uniqueIndex:uk_follower_following,priority:1"`
	FollowingID int64     `gorm:"column:following_id;not null;uniqueIndex:uk_follower_following,priority:2;index:idx_following_id"`
	CreatedAt   time.Time `gorm:"column:created_at;type:datetime(3);not null;default:CURRENT_TIMESTAMP(3)"`
}

// TableName tells GORM the explicit table name. Explicit is safer than the
// pluralisation default when refactoring.
func (Follow) TableName() string {
	return "follows"
}