// Package model — post.go defines the GORM model that maps 1-to-1 to the
// `posts` table created by migrations/000002_create_posts_table.up.sql.
//
// Field naming follows snake_case in DB and PascalCase in Go (GORM convention).
// Persistence-layer details (column types, index definitions, constraints)
// live in the SQL migration; the migration is the source of truth. GORM tags
// here describe per-column type/size/default for safety, but they are NOT
// used to create the table (we never call AutoMigrate).
//
// Counter fields (LikeCount / ViewCount) are eventually-consistent views
// maintained by LikeConsumer (Step 5) and PVConsumer (Step 5). Request
// handlers MUST NOT update them directly — that races with the consumer
// and produces lost updates.
//
// is_visible / audit_status are maintained by AuditConsumer (Step 10).
// MVP (Step 4) writes is_visible=1 + audit_status=0 directly so users see
// their posts immediately. Step 10 flips this: new posts get is_visible=0
// and the consumer flips them to 1 after machine review. The change is
// transparent to readers because Feed queries always use `WHERE is_visible=1`.
//
// Future improvements:
//   - When post_steps subtable lands (Step 9 image upload), Content remains
//     here as a legacy short-text fallback; structured steps move out.
//   - cook_duration is a small bucketed enum today; if exact-minutes display
//     becomes useful, add `cook_minutes INT UNSIGNED` and treat
//     cook_duration as a derived bucket (or drop it).
//   - Hot posts may eventually need per-row caching keyed by post_id; for
//     now Feed-level caching covers the read path.
//
// Added in Step 4 (content module).
package model

import (
	"time"

	"gorm.io/gorm"
)

// Post is the GORM-managed entity for the posts table.
type Post struct {
	ID           int64          `gorm:"primaryKey;column:id"`
	UserID       int64          `gorm:"column:user_id;not null"`
	Title        string         `gorm:"column:title;size:100;not null"`
	Content      string         `gorm:"column:content;type:text"`
	SceneTag     SceneTag       `gorm:"column:scene_tag;type:tinyint unsigned;not null"`
	CookDuration uint8          `gorm:"column:cook_duration;type:tinyint unsigned;not null;default:0"`
	CoverURL     string         `gorm:"column:cover_url;size:500;not null;default:''"`
	LikeCount    uint32         `gorm:"column:like_count;not null;default:0"`
	ViewCount    uint32         `gorm:"column:view_count;not null;default:0"`
	IsVisible    uint8          `gorm:"column:is_visible;type:tinyint unsigned;not null;default:0"`
	AuditStatus  uint8          `gorm:"column:audit_status;type:tinyint unsigned;not null;default:0"`
	AuditRemark  string         `gorm:"column:audit_remark;size:200;not null;default:''"`
	CreatedAt    time.Time      `gorm:"column:created_at;type:datetime(3);not null;default:CURRENT_TIMESTAMP(3)"`
	UpdatedAt    time.Time      `gorm:"column:updated_at;type:datetime(3);not null;default:CURRENT_TIMESTAMP(3)"`
	DeletedAt    gorm.DeletedAt `gorm:"column:deleted_at;type:datetime(3);index"`
}

// TableName returns the explicit MySQL table name. GORM would pluralise to
// "posts" anyway, but explicit beats implicit when refactoring or migrating
// to another DB engine.
func (Post) TableName() string {
	return "posts"
}

// AuditStatus enum values. Untyped const so callers can compare directly
// against Post.AuditStatus (uint8) without conversion.
//
// Values match posts.audit_status COMMENT in the migration. Do not reorder
// or reuse — see scene_tag.go's evolution rules; same logic applies here.
const (
	AuditStatusPending     uint8 = 0 // 待审（默认值；机审尚未回调）
	AuditStatusMachinePass uint8 = 1 // 机审通过 → is_visible=1
	AuditStatusSuspect     uint8 = 2 // 机审疑似 → 进人工队列，is_visible=0
	AuditStatusManualPass  uint8 = 3 // 人工通过 → is_visible=1
	AuditStatusMachineDeny uint8 = 4 // 机审拒绝 → is_visible=0
	AuditStatusManualDeny  uint8 = 5 // 人工拒绝 → is_visible=0
)

// is_visible literal values. Centralised here so handler/service code never
// hardcodes 0/1.
const (
	PostInvisible uint8 = 0
	PostVisible   uint8 = 1
)
