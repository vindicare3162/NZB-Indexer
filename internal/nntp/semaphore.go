package nntp

import (
	"context"
	"sync"
)

// resizableSem is a counting semaphore whose capacity (limit) can be changed at
// runtime without interrupting in-flight holders or ever letting the number of
// concurrent holders exceed the CURRENT limit.
//
// It replaces a fixed-capacity buffered channel so the NNTP pool's connection
// ceiling can track admin changes to a server's max-connections (#111):
//
//   - Increasing the limit immediately wakes any waiters up to the new count.
//   - Decreasing the limit stops granting new tokens beyond the new limit;
//     existing holders finish and release naturally, so inUse converges down to
//     the new limit without interrupting anyone or exceeding the provider's
//     budget at any instant (inUse never rises above the current limit, and a
//     shrink never revokes a token already held).
type resizableSem struct {
	mu    sync.Mutex
	cond  *sync.Cond
	limit int
	inUse int
}

func newResizableSem(limit int) *resizableSem {
	if limit < 1 {
		limit = 1
	}
	s := &resizableSem{limit: limit}
	s.cond = sync.NewCond(&s.mu)
	return s
}

// acquire blocks until a slot is available (inUse < limit) or ctx is done.
// On success it increments inUse and returns nil; the caller must call release.
func (s *resizableSem) acquire(ctx context.Context) error {
	// Fast path + context-aware wait. sync.Cond can't select on ctx, so we run
	// a watcher goroutine that broadcasts on cancellation to wake the waiter.
	s.mu.Lock()
	for s.inUse >= s.limit {
		if err := ctx.Err(); err != nil {
			s.mu.Unlock()
			return err
		}
		// Arrange a wakeup when ctx is cancelled.
		done := make(chan struct{})
		stop := context.AfterFunc(ctx, func() {
			s.mu.Lock()
			s.cond.Broadcast()
			s.mu.Unlock()
			close(done)
		})
		s.cond.Wait()
		// Cancel the watcher; if it already fired, draining is unnecessary
		// because it closed over its own done channel.
		stop()
	}
	if err := ctx.Err(); err != nil {
		s.mu.Unlock()
		return err
	}
	s.inUse++
	s.mu.Unlock()
	return nil
}

// release returns a slot and wakes one waiter.
func (s *resizableSem) release() {
	s.mu.Lock()
	if s.inUse > 0 {
		s.inUse--
	}
	s.cond.Signal()
	s.mu.Unlock()
}

// resize changes the capacity. Growing wakes waiters; shrinking takes effect
// for future acquisitions only (current holders are never revoked, so inUse can
// briefly exceed the new limit until holders release, but it never exceeds the
// OLD limit either — i.e. the provider budget is never breached because the old
// limit was already honoured).
func (s *resizableSem) resize(n int) {
	if n < 1 {
		n = 1
	}
	s.mu.Lock()
	grew := n > s.limit
	s.limit = n
	if grew {
		s.cond.Broadcast()
	}
	s.mu.Unlock()
}

// stats returns the current limit and in-use count.
func (s *resizableSem) stats() (limit, inUse int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.limit, s.inUse
}
