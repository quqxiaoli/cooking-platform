// Package consumer — helper.go provides shared utilities for all consumers.
package consumer

// DrainChan drains all buffered items from ch into fn until the channel is empty.
//
// Must be called only after the sender goroutines have exited (e.g. after
// subWg.Wait()), so no new items will arrive while draining. ch is never
// closed — we rely on the default branch to detect an empty channel.
//
// This replaces the copy-pasted labeled drainLoop blocks that appeared in
// like_consumer, pv_consumer, and count_consumer (Step 13 extraction).
func DrainChan[T any](ch <-chan T, fn func(T)) {
	for {
		select {
		case item := <-ch:
			fn(item)
		default:
			return
		}
	}
}
