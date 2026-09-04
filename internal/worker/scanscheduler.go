package worker

import "context"

// Scan scheduling with forward priority (#112).
//
// Forward scans (indexing newly posted content) must not be delayed
// indefinitely by a large historical backfill. The scheduler serializes all
// scan work through a single caller (so no group is ever scanned forward and
// backward at once), but always drains FORWARD demand before doing backfill,
// and runs backfill one group at a time so an overdue forward pass preempts the
// remaining backfill groups after the current group finishes.
//
// The scheduler is deliberately decoupled from the store and NNTP client via
// the scanRunner interface, so its fairness/priority behaviour can be tested
// deterministically without a database.

// scanRunner performs the actual scan work. runForward scans all due forward
// groups (one full pass); listBackfillGroups enumerates the groups eligible for
// backfill; scanBackfillGroup backfills a single group.
type scanRunner interface {
	runForward(ctx context.Context, group string)
	listBackfillGroups(ctx context.Context) []string
	scanBackfillGroup(ctx context.Context, group string)
}

// scanScheduler decides what scan work to run next, giving forward passes
// strict priority over backfill and yielding backfill between groups whenever
// forward work becomes due.
type scanScheduler struct {
	runner scanRunner
	// forwardDue reports whether a forward pass should run now (ticker fired or
	// a manual trigger is pending). Consulted between backfill groups so a due
	// forward pass preempts remaining backfill.
	forwardDue func() bool
	// backfillEnabled reports whether backfill should run at all.
	backfillEnabled func() bool
}

// runForwardPass runs a single forward pass for the given group ("" = all).
func (s *scanScheduler) runForwardPass(ctx context.Context, group string) {
	s.runner.runForward(ctx, group)
}

// drainBackfill runs backfill one group at a time, yielding to forward work
// between groups. It returns as soon as forward becomes due (so the caller can
// run the higher-priority forward pass), when ctx is cancelled, or when all
// backfill groups have been serviced. Returns true if it yielded to forward
// (i.e. there is more backfill to do), false if it drained fully.
func (s *scanScheduler) drainBackfill(ctx context.Context) (yielded bool) {
	if s.backfillEnabled != nil && !s.backfillEnabled() {
		return false
	}
	groups := s.runner.listBackfillGroups(ctx)
	for _, g := range groups {
		if ctx.Err() != nil {
			return false
		}
		// Yield BEFORE starting each group if a forward pass is due, so forward
		// work runs promptly rather than after the whole backfill completes.
		if s.forwardDue != nil && s.forwardDue() {
			return true
		}
		s.scanBackfillGroupChecked(ctx, g)
	}
	return false
}

func (s *scanScheduler) scanBackfillGroupChecked(ctx context.Context, group string) {
	if ctx.Err() != nil {
		return
	}
	s.runner.scanBackfillGroup(ctx, group)
}
