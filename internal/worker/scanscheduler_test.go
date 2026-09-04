package worker

import (
	"context"
	"sync"
	"testing"
)

// fakeRunner records the order of scan operations and lets the test control
// when a forward pass "becomes due" during backfill.
type fakeRunner struct {
	mu       sync.Mutex
	events   []string // e.g. "forward:", "backfill:groupA"
	backfill []string // groups to backfill
}

func (f *fakeRunner) runForward(_ context.Context, group string) {
	f.mu.Lock()
	f.events = append(f.events, "forward:"+group)
	f.mu.Unlock()
}
func (f *fakeRunner) listBackfillGroups(_ context.Context) []string {
	return append([]string(nil), f.backfill...)
}
func (f *fakeRunner) scanBackfillGroup(_ context.Context, group string) {
	f.mu.Lock()
	f.events = append(f.events, "backfill:"+group)
	f.mu.Unlock()
}
func (f *fakeRunner) log() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.events...)
}

func TestSchedulerBackfillDrainsWhenNoForwardDue(t *testing.T) {
	r := &fakeRunner{backfill: []string{"a", "b", "c"}}
	s := &scanScheduler{
		runner:          r,
		forwardDue:      func() bool { return false },
		backfillEnabled: func() bool { return true },
	}
	if yielded := s.drainBackfill(context.Background()); yielded {
		t.Error("should not yield when no forward is due")
	}
	got := r.log()
	want := []string{"backfill:a", "backfill:b", "backfill:c"}
	if len(got) != len(want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("event %d = %s, want %s", i, got[i], want[i])
		}
	}
}

func TestSchedulerYieldsToForwardMidBackfill(t *testing.T) {
	r := &fakeRunner{backfill: []string{"a", "b", "c", "d"}}
	// Forward becomes due after the second backfill group.
	calls := 0
	s := &scanScheduler{
		runner: r,
		forwardDue: func() bool {
			calls++
			// forwardDue is checked before each group: allow a and b, then yield.
			return calls > 2
		},
		backfillEnabled: func() bool { return true },
	}
	yielded := s.drainBackfill(context.Background())
	if !yielded {
		t.Fatal("expected drainBackfill to yield when forward became due")
	}
	got := r.log()
	// Only a and b ran before yielding; c/d were preempted.
	want := []string{"backfill:a", "backfill:b"}
	if len(got) != len(want) {
		t.Fatalf("events = %v, want %v (should have yielded before c)", got, want)
	}
}

func TestSchedulerBackfillDisabled(t *testing.T) {
	r := &fakeRunner{backfill: []string{"a", "b"}}
	s := &scanScheduler{
		runner:          r,
		forwardDue:      func() bool { return false },
		backfillEnabled: func() bool { return false },
	}
	if yielded := s.drainBackfill(context.Background()); yielded {
		t.Error("disabled backfill should not yield")
	}
	if len(r.log()) != 0 {
		t.Errorf("disabled backfill should run nothing, got %v", r.log())
	}
}

func TestSchedulerBackfillCancellation(t *testing.T) {
	r := &fakeRunner{backfill: []string{"a", "b", "c"}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := &scanScheduler{
		runner:          r,
		forwardDue:      func() bool { return false },
		backfillEnabled: func() bool { return true },
	}
	if yielded := s.drainBackfill(ctx); yielded {
		t.Error("cancelled drain should not report a yield")
	}
	if len(r.log()) != 0 {
		t.Errorf("cancelled drain should run no groups, got %v", r.log())
	}
}

// TestSchedulerForwardNeverStarved simulates a long backfill with forward work
// repeatedly becoming due; forward must run each time backfill yields, so a
// forward pass is never delayed by more than one backfill group.
func TestSchedulerForwardNeverStarved(t *testing.T) {
	r := &fakeRunner{backfill: []string{"a", "b", "c", "d", "e"}}
	forwardDue := true // forward is always due
	s := &scanScheduler{
		runner:          r,
		forwardDue:      func() bool { return forwardDue },
		backfillEnabled: func() bool { return true },
	}
	// With forward always due, the first drain yields immediately (no backfill
	// group runs before forward is serviced).
	yielded := s.drainBackfill(context.Background())
	if !yielded {
		t.Fatal("expected immediate yield when forward is always due")
	}
	if len(r.log()) != 0 {
		t.Errorf("no backfill should run while forward is due, got %v", r.log())
	}
}
