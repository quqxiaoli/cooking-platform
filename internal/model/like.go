// Package model — like.go defines the GORM model that maps 1-to-1 to the
// `likes` table created by migrations/000003_create_likes_table.up.sql.
//
// Field naming follows snake_case in DB and PascalCase in Go (GORM convention).
// As with post.go and user.go, the SQL migration is the source of truth for
// column types, constraints and indexes; GORM tags here are descriptive only,
// never used to create the table (we never call AutoMigrate).
//
// ── Why no DeletedAt field ──────────────────────────────────────────────────
//
// Likes are physically deleted on Unlike, not soft-deleted. See the SQL
// migration's note (1) for the rationale: soft-delete plus uk_user_post
// would either silently swallow re-likes or require composite uniqueness
// including deleted_at, both worse than physical delete + INSERT IGNORE.
//
// ── Why no UpdatedAt ────────────────────────────────────────────────────────
//
// A like row is immutable: once created it either exists (user has liked)
// or doesn't (user has not / has unliked). Nothing about the row changes
// after insert, so updated_at would be dead weight on every row.
//
// Added in Step 5 (like module).
package model

import "time"

// Like is the GORM-managed entity for the likes table.
type Like struct {
	ID        int64     `gorm:"primaryKey;column:id"`
	UserID    int64     `gorm:"column:user_id;not null;uniqueIndex:uk_user_post,priority:1"`
	PostID    int64     `gorm:"column:post_id;not null;uniqueIndex:uk_user_post,priority:2;index:idx_post_id"`
	CreatedAt time.Time `gorm:"column:created_at;type:datetime(3);not null;default:CURRENT_TIMESTAMP(3)"`
}

// TableName returns the explicit MySQL table name. GORM would pluralise
// "Like" to "likes" anyway, but explicit beats implicit when refactoring
// or migrating to another DB engine.
func (Like) TableName() string {
	return "likes"
}
