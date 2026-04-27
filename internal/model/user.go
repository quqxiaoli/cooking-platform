// Package model — user.go defines the GORM model that maps 1-to-1 to the
// `users` table created by migrations/000001_create_users_table.up.sql.
//
// Field naming follows snake_case in DB and PascalCase in Go (GORM convention).
// All persistence-layer details (column types, indices, constraints) live in
// the SQL migration, not in struct tags — the migration is the source of truth.
//
// Added in Step 3 (user module).
package model

import (
	"time"

	"gorm.io/gorm"
)

// UserStatus is a typed alias to prevent passing arbitrary integers where
// status is expected. Both the DB column and DTO fields use this type.
type UserStatus uint8

const (
	UserStatusNormal UserStatus = 0
	UserStatusBanned UserStatus = 1
)

// User is the GORM-managed entity for the users table.
//
// Soft delete: the DeletedAt field is automatically respected by GORM.
// All queries via gorm.DB will exclude rows with non-null deleted_at unless
// .Unscoped() is used. We do not expose Unscoped() outside the repository.
//
// Counter fields (PostCount/TotalLikes/FollowerCount/FollowingCount) are
// maintained by CountConsumer (Step 5+) and treated as eventually-consistent
// read-only views from the application layer. Never UPDATE these directly
// from request handlers — that would race with the consumer.
type User struct {
	ID             int64          `gorm:"primaryKey;column:id"`
	PhoneHash      string         `gorm:"column:phone_hash;size:64;uniqueIndex:uk_phone_hash;not null"`
	PhoneEncrypted string         `gorm:"column:phone_encrypted;size:200;not null"`
	Nickname       string         `gorm:"column:nickname;size:50;not null;default:''"`
	AvatarURL      string         `gorm:"column:avatar_url;size:500;not null;default:''"`
	Bio            string         `gorm:"column:bio;size:200;not null;default:''"`
	Status         UserStatus     `gorm:"column:status;type:tinyint unsigned;not null;default:0"`
	PostCount      uint32         `gorm:"column:post_count;not null;default:0"`
	TotalLikes     uint32         `gorm:"column:total_likes;not null;default:0"`
	FollowerCount  uint32         `gorm:"column:follower_count;not null;default:0"`
	FollowingCount uint32         `gorm:"column:following_count;not null;default:0"`
	CreatedAt      time.Time      `gorm:"column:created_at;type:datetime(3);not null;default:CURRENT_TIMESTAMP(3)"`
	UpdatedAt      time.Time      `gorm:"column:updated_at;type:datetime(3);not null;default:CURRENT_TIMESTAMP(3)"`
	DeletedAt      gorm.DeletedAt `gorm:"column:deleted_at;type:datetime(3);index"`
}

// TableName tells GORM the explicit table name (otherwise it would pluralise
// to "users" by default — same result here, but explicit is safer when refactoring).
func (User) TableName() string {
	return "users"
}

// IsBanned is a small convenience helper used by the Auth middleware so
// callers don't have to remember the magic constant.
func (u *User) IsBanned() bool {
	return u.Status == UserStatusBanned
}
