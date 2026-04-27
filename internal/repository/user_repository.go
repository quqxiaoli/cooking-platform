// Package repository — user_repository.go is the data-access layer for
// the User entity. All MySQL I/O for users goes through this file.
//
// The repository is intentionally thin: each method maps to one or two SQL
// statements, with no business logic. Business rules (e.g. "ban check before
// login") live in the service layer.
//
// All methods accept context.Context for cancellation and tracing. They do
// NOT accept *gorm.DB explicitly — callers needing transactional behaviour
// will be addressed in Step 4 (post creation needs cross-table writes).
//
// Added in Step 3 (user module).
package repository

import (
	"context"
	"errors"

	"cooking-platform/internal/model"

	"gorm.io/gorm"
)

// ErrUserNotFound is returned when a query expects exactly one user but
// none is found. It is a sentinel error — service layer maps it to
// errcode.ErrUserNotFound for the HTTP response.
var ErrUserNotFound = errors.New("user not found")

// UserRepository is the abstraction the service layer depends on. Defined
// here (where the implementation lives) for now; if multiple implementations
// emerge later, we can move it to its own file or package.
type UserRepository interface {
	FindByPhoneHash(ctx context.Context, phoneHash string) (*model.User, error)
	FindByID(ctx context.Context, id int64) (*model.User, error)
	Create(ctx context.Context, u *model.User) error
	UpdateProfile(ctx context.Context, id int64, fields map[string]interface{}) error
}

// userRepository is the GORM-backed implementation of UserRepository.
// Lowercase by design — callers depend on the interface, not the struct.
type userRepository struct {
	db *gorm.DB
}

// NewUserRepository constructs a GORM-backed UserRepository.
func NewUserRepository(db *gorm.DB) UserRepository {
	return &userRepository{db: db}
}

// FindByPhoneHash looks up a user by their hashed phone number, the canonical
// "find user by login identity" operation.
//
// Returns ErrUserNotFound if no row matches; other errors are wrapped DB errors.
// Soft-deleted rows are excluded automatically by GORM.
func (r *userRepository) FindByPhoneHash(ctx context.Context, phoneHash string) (*model.User, error) {
	var u model.User
	err := r.db.WithContext(ctx).
		Where("phone_hash = ?", phoneHash).
		First(&u).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrUserNotFound
		}
		return nil, err
	}
	return &u, nil
}

// FindByID looks up a user by primary key. Used by the Auth middleware (after
// JWT validation) and by GET /users/:id.
func (r *userRepository) FindByID(ctx context.Context, id int64) (*model.User, error) {
	var u model.User
	err := r.db.WithContext(ctx).First(&u, id).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrUserNotFound
		}
		return nil, err
	}
	return &u, nil
}

// Create inserts a new user row. The User's ID is populated on success
// (GORM sets it from LAST_INSERT_ID). PhoneHash uniqueness is enforced at
// the DB level — callers should treat duplicate-key errors as "user exists"
// and react accordingly (typically: load existing user and proceed).
func (r *userRepository) Create(ctx context.Context, u *model.User) error {
	return r.db.WithContext(ctx).Create(u).Error
}

// UpdateProfile applies a partial update to the user's mutable profile fields.
//
// Using map[string]interface{} (rather than a struct) lets the service layer
// pass only the fields the client explicitly set. GORM's Updates() with a map
// only writes provided keys, leaving others untouched and avoiding the
// "zero-value overwrite" trap that plagues struct-based updates.
//
// The caller is responsible for whitelisting which keys may be updated —
// this method blindly trusts the input. We never expose the underlying
// gorm.DB to the handler layer, so this trust boundary is correct.
func (r *userRepository) UpdateProfile(ctx context.Context, id int64, fields map[string]interface{}) error {
	if len(fields) == 0 {
		return nil
	}
	return r.db.WithContext(ctx).
		Model(&model.User{}).
		Where("id = ?", id).
		Updates(fields).Error
}
