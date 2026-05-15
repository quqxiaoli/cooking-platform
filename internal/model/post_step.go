// Package model — post_step.go is the GORM model for the post_steps subtable
// introduced in Step 9 to back PRD-Phase2 §F-C01 structured steps.
//
// Field naming follows the project's existing convention (see post.go):
// snake_case in DB, PascalCase in Go, GORM tags describe column type and
// constraints. The migration is the source of truth — these tags are
// documentation that survive a DB engine swap, not table-creation directives
// (we never call AutoMigrate).
//
// ── Why a custom StringArray type ──────────────────────────────────────────
//
// image_urls is a JSON column. Three options were considered:
//
//	A) Store as `string` in Go, marshal/unmarshal in every service method.
//	   Pros: zero infra code.
//	   Cons: every read/write site repeats the same json.Marshal dance;
//	         inconsistent error handling leaks into business logic.
//
//	B) Use gorm.io/datatypes.JSON (a third-party type alias).
//	   Pros: out-of-the-box.
//	   Cons: extra dependency, exposes raw bytes — callers still have to
//	         unmarshal to []string at the boundary.
//
//	C) Define StringArray ([]string with Scan/Value).
//	   Pros: callers see []string everywhere — append, range, len, JSON
//	         output all work natively. Marshal/unmarshal happens at the
//	         DB boundary exactly once, in code that has no business logic
//	         to muddle the error path.
//	   Cons: ~30 LOC to write.
//
// We pick C. The cost is one-time; the ergonomic win is permanent. Tests
// at the service layer no longer need to JSON-escape image URL lists.
//
// Added in Step 9 (image upload module).
package model

import (
	"database/sql/driver"
	"encoding/json"
	"errors"
	"time"
)

// StringArray is a []string backed by a MySQL JSON column.
//
// Always serialises as a JSON array, even when the slice is nil — emitting
// "[]" rather than "null" keeps the DB column NOT NULL clean and lets
// callers range over the value without nil checks.
type StringArray []string

// Scan converts the raw bytes read from the JSON column into the slice.
// A nil source or empty payload becomes an empty (non-nil) slice.
func (s *StringArray) Scan(src any) error {
	if src == nil {
		*s = StringArray{}
		return nil
	}
	var b []byte
	switch v := src.(type) {
	case []byte:
		b = v
	case string:
		b = []byte(v)
	default:
		return errors.New("model.StringArray: unsupported scan type")
	}
	if len(b) == 0 {
		*s = StringArray{}
		return nil
	}
	return json.Unmarshal(b, s)
}

// Value serialises the slice to JSON bytes for the DB driver.
// A nil slice serialises as "[]" so the column never holds JSON null.
func (s StringArray) Value() (driver.Value, error) {
	if s == nil {
		return []byte("[]"), nil
	}
	return json.Marshal([]string(s))
}

// PostStep is the GORM-managed entity for the post_steps table.
//
// One post owns 0..30 PostStep rows. Empty step list = legacy text-only
// post (Content field on model.Post carries the body). Both layouts coexist
// indefinitely — see post.go header for the migration philosophy.
type PostStep struct {
	ID        int64       `gorm:"primaryKey;column:id"`
	PostID    int64       `gorm:"column:post_id;not null"`
	StepNo    uint8       `gorm:"column:step_no;type:tinyint unsigned;not null"`
	Text      string      `gorm:"column:text;size:500;not null;default:''"`
	ImageURLs StringArray `gorm:"column:image_urls;type:json;not null"`
	CreatedAt time.Time   `gorm:"column:created_at;type:datetime(3);not null;default:CURRENT_TIMESTAMP(3)"`
}

// TableName returns the explicit MySQL table name. Same rationale as
// Post.TableName: explicit beats implicit when refactoring.
func (PostStep) TableName() string {
	return "post_steps"
}
