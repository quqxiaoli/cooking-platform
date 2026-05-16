// Package model — audit_log.go maps to the audit_logs table created by
// migrations/000006_create_audit_logs_table.up.sql.
//
// AuditLog rows are append-only: AuditConsumer inserts one row per state
// transition and never updates existing rows. The table is a compliance
// audit trail, not a mutable status column (that lives on posts.audit_status).
//
// Added in Step 10 (content moderation).
package model

import "time"

// AuditLog is the GORM model for the audit_logs table.
type AuditLog struct {
	ID          int64     `gorm:"primaryKey;column:id"`
	PostID      int64     `gorm:"column:post_id;not null"`
	AuthorID    int64     `gorm:"column:author_id;not null"`
	AuditStatus uint8     `gorm:"column:audit_status;type:tinyint unsigned;not null"`
	Remark      string    `gorm:"column:remark;size:500;not null;default:''"`
	RawResponse string    `gorm:"column:raw_response;type:text;not null"`
	CreatedAt   time.Time `gorm:"column:created_at;type:datetime(3);not null;default:CURRENT_TIMESTAMP(3)"`
}

// TableName returns the explicit table name.
func (AuditLog) TableName() string {
	return "audit_logs"
}
