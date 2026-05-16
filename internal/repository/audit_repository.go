// Package repository — audit_repository.go is the data-access layer for
// audit_logs. The interface is intentionally minimal: audit records are
// append-only, so only Create is exposed. No FindByID, no List — those
// belong to a future admin module.
//
// Added in Step 10 (content moderation).
package repository

import (
	"context"

	"cooking-platform/internal/model"

	"gorm.io/gorm"
)

// AuditRepository abstracts writes to the audit_logs table.
type AuditRepository interface {
	// Create appends one audit event row. Never call Update or Delete on
	// audit_logs — the table is a compliance trail.
	Create(ctx context.Context, log *model.AuditLog) error
}

type auditRepository struct {
	db *gorm.DB
}

// NewAuditRepository constructs a GORM-backed AuditRepository.
func NewAuditRepository(db *gorm.DB) AuditRepository {
	return &auditRepository{db: db}
}

func (r *auditRepository) Create(ctx context.Context, log *model.AuditLog) error {
	return r.db.WithContext(ctx).Create(log).Error
}
