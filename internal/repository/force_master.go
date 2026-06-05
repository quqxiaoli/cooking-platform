// Package repository — force_master.go provides a context-scoped hint that
// tells GORM to route the query at hand to the master DB rather than a slave.
// ([Fix #1] read-after-write — see internal/cache/write_marker.go for the
// problem statement.)
//
// Why context-based, not method-based:
//   We didn't want to fork every read method into a "ForceMaster" sibling
//   (ListByUserForceMaster, FindByIDForceMaster, ...) — that explodes the
//   interface surface for one bit of routing metadata. A context value is
//   the lighter alternative: the service layer decides per-request whether
//   the read needs the master, and the repository implementations probe the
//   hint once via Apply.
//
// Usage:
//
//	// service
//	if writeMarker.Has(ctx, authorID) {
//	    ctx = repository.WithForceMaster(ctx)
//	}
//	posts, err := postRepo.ListByUser(ctx, authorID, ...)
//
//	// repository
//	db := repository.Apply(ctx, r.db)
//	return db.Where(...).Find(...)
//
// Pure pass-through when no hint set (and when DBResolver is disabled in dev
// — Clauses(dbresolver.Write) is a no-op without the plugin).
package repository

import (
	"context"

	"gorm.io/gorm"
	"gorm.io/plugin/dbresolver"
)

type forceMasterKey struct{}

// WithForceMaster returns a derived context that signals to Apply (below) that
// any GORM query running with this context should be routed to the master DB.
func WithForceMaster(ctx context.Context) context.Context {
	return context.WithValue(ctx, forceMasterKey{}, true)
}

// IsForceMaster reports whether the context carries the WithForceMaster hint.
// Exposed for tests / observability — production code typically goes through
// Apply directly.
func IsForceMaster(ctx context.Context) bool {
	v, _ := ctx.Value(forceMasterKey{}).(bool)
	return v
}

// Apply binds the context to db.WithContext and conditionally appends
// dbresolver.Write so the query is force-routed to the master. Repositories
// call this in place of db.WithContext(ctx) for any read where stale reads
// would surface as a user-visible bug.
//
// When DBResolver is not registered (dev with no slaves_dsn), the Clauses call
// is silently ignored — Apply degrades to plain WithContext.
func Apply(ctx context.Context, db *gorm.DB) *gorm.DB {
	sess := db.WithContext(ctx)
	if IsForceMaster(ctx) {
		sess = sess.Clauses(dbresolver.Write)
	}
	return sess
}
