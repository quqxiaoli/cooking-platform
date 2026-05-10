// Package consumer contains event consumers that process asynchronous messages
// from the EventBus and persist their side-effects to MySQL.
//
// Consumer roster (all added in Step 5+):
//
//	manager.go        — lifecycle management: Start, Shutdown (graceful drain)
//	like_consumer.go  — like/unlike events → likes table + posts.like_count
//	pv_consumer.go    — page-view events → posts.view_count (batch, 100 msgs or 5s)
//	audit_consumer.go — audit result events → posts.audit_status + is_visible
//	count_consumer.go — aggregated count events → users redundant counters
package consumer
