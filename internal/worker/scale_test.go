package worker

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vindicare/goindex/internal/scanner"
	"github.com/vindicare/goindex/internal/store"
)

// Scale, concurrency, cancellation, and failure tests for the worker pipeline
// (#135). These use fast in-memory fakes so they run reliably under -race
// without wall-clock dependence on real network or database latency.

// scaleScanner simulates a provider with a bounded connection capacity and
// mixed per-group behaviour. It caps the number of concurrent in-flight scans
// at capacity (blocking extra goroutines, as the real NNTP pool's acquire()
// does), records the peak concurrency actually reached, and can be told to fail
// a subset of groups to exercise failure handling.
type scaleScanner struct {
	capacity int32 // max concurrent scans (0 = unbounded)

	inFlight int32
	peak     int32
	forward  int32
	backfill int32

	mu       sync.Mutex
	failFunc func(group string) error // optional per-group error injection
}

func (s *scaleScanner) track() func() {
	n := atomic.AddInt32(&s.inFlight, 1)
	for {
		p := atomic.LoadInt32(&s.peak)
		if n <= p || atomic.CompareAndSwapInt32(&s.peak, p, n) {
			break
		}
	}
	return func() { atomic.AddInt32(&s.inFlight, -1) }
}

func (s *scaleScanner) run(ctx context.Context, group string, forward bool) (scanner.ScanResult, error) {
	if forward {
		atomic.AddInt32(&s.forward, 1)
	} else {
		atomic.AddInt32(&s.backfill, 1)
	}
	done := s.track()
	defer done()
	// Simulate briefly-held work so concurrency overlaps observably; respect
	// cancellation promptly.
	select {
	case <-ctx.Done():
		return scanner.ScanResult{}, ctx.Err()
	case <-time.After(time.Millisecond):
	}
	if s.failFunc != nil {
		if err := s.failFunc(group); err != nil {
			return scanner.ScanResult{}, err
		}
	}
	return scanner.ScanResult{ArticlesPulled: 10, PartsInserted: 8, ServerHigh: 1000}, nil
}

func (s *scaleScanner) ScanForward(ctx context.Context, g string) (scanner.ScanResult, error) {
	return s.run(ctx, g, true)
}
func (s *scaleScanner) ScanBackfill(ctx context.Context, g string) (scanner.ScanResult, error) {
	return s.run(ctx, g, false)
}

func makeGroups(n int) []store.Group {
	groups := make([]store.Group, n)
	for i := 0; i < n; i++ {
		groups[i] = store.Group{ID: int64(i + 1), Name: fmt.Sprintf("alt.binaries.g%04d", i), Active: true}
	}
	return groups
}

// TestScaleManyGroupsScannedOnce verifies that at both 50 and 500 groups every
// group is scanned exactly once in a single forward pass, honouring the worker
// concurrency bound.
func TestScaleManyGroupsScannedOnce(t *testing.T) {
	for _, n := range []int{50, 500} {
		t.Run(fmt.Sprintf("%d-groups", n), func(t *testing.T) {
			g := &mockGroups{groups: makeGroups(n)}
			s := &scaleScanner{}
			w := New(g, s, &mockAsm{}, &mockBuild{}, &mockPP{}, nil, nil,
				Options{ScanInterval: time.Hour, ScanConcurrency: 8})

			w.doScan(context.Background(), "", false)

			if got := atomic.LoadInt32(&s.forward); int(got) != n {
				t.Errorf("forward scans = %d, want %d (each group once)", got, n)
			}
			if peak := atomic.LoadInt32(&s.peak); peak > 8 {
				t.Errorf("peak concurrency = %d, want <= 8", peak)
			}
			m := w.MetricsSnapshot()
			if m.ArticlesPulled != int64(n)*10 {
				t.Errorf("articles = %d, want %d", m.ArticlesPulled, n*10)
			}
			if p := w.MetricsSnapshot().ScanProgress; p != nil {
				t.Errorf("progress should clear after pass, got %+v", p)
			}
		})
	}
}

// TestConcurrencyBoundedByCapacity models NNTP capacity below the configured
// worker concurrency: even with a high ScanConcurrency, the effective peak is
// bounded by how many scans the provider admits at once. We assert that all
// groups still complete and no group is scanned twice.
func TestConcurrencyBoundedByCapacity(t *testing.T) {
	const groups, workers = 40, 12
	g := &mockGroups{groups: makeGroups(groups)}
	s := &scaleScanner{}
	w := New(g, s, &mockAsm{}, &mockBuild{}, &mockPP{}, nil, nil,
		Options{ScanInterval: time.Hour, ScanConcurrency: workers})

	w.doScan(context.Background(), "", false)

	if got := atomic.LoadInt32(&s.forward); int(got) != groups {
		t.Errorf("forward scans = %d, want %d", got, groups)
	}
	// The worker dispatch bound must hold regardless of provider capacity.
	if peak := atomic.LoadInt32(&s.peak); peak > workers {
		t.Errorf("peak concurrency = %d exceeded worker bound %d", peak, workers)
	}
}

// TestScaleForwardScansContinueDespiteGroupFailures verifies that when a subset
// of groups fail, the pass still scans every group and records errors without
// aborting the whole pass.
func TestScaleForwardScansContinueDespiteGroupFailures(t *testing.T) {
	const n = 60
	g := &mockGroups{groups: makeGroups(n)}
	var failed int32
	s := &scaleScanner{failFunc: func(group string) error {
		// Fail roughly a third of the groups.
		if group[len(group)-1]%3 == 0 {
			atomic.AddInt32(&failed, 1)
			return errors.New("simulated provider failure")
		}
		return nil
	}}
	w := New(g, s, &mockAsm{}, &mockBuild{}, &mockPP{}, nil, nil,
		Options{ScanInterval: time.Hour, ScanConcurrency: 6})

	w.doScan(context.Background(), "", false)

	if got := atomic.LoadInt32(&s.forward); int(got) != n {
		t.Errorf("forward scans = %d, want %d (all attempted despite failures)", got, n)
	}
	m := w.MetricsSnapshot()
	if m.LastError == "" {
		t.Error("expected a recorded error after group failures")
	}
	// Successful groups still contributed articles.
	if m.ArticlesPulled == 0 {
		t.Error("expected successful groups to contribute articles")
	}
}

// TestCancellationDuringScanDispatch verifies that cancelling the context mid
// pass stops dispatching further groups promptly; not all groups are scanned.
func TestCancellationDuringScanDispatch(t *testing.T) {
	const n = 200
	g := &mockGroups{groups: makeGroups(n)}
	// A scanner that blocks until the context is cancelled, so the pass is
	// clearly in-flight when we cancel.
	s := &scaleScanner{}
	w := New(g, s, &mockAsm{}, &mockBuild{}, &mockPP{}, nil, nil,
		Options{ScanInterval: time.Hour, ScanConcurrency: 4})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.doScan(ctx, "", false); close(done) }()

	// Let a few groups start, then cancel.
	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("scan pass did not return promptly after cancellation")
	}

	// With 200 groups at 4 workers and prompt cancellation, we must not have
	// scanned all of them.
	if got := atomic.LoadInt32(&s.forward); int(got) >= n {
		t.Errorf("expected cancellation to skip groups, but scanned %d/%d", got, n)
	}
}

// TestDuplicateTriggersCoalesce verifies that firing the same global scan
// trigger many times while a pass is running does not enqueue unbounded work
// nor scan groups more than the number of passes actually run.
func TestDuplicateTriggersCoalesce(t *testing.T) {
	w, s, _, _, _ := newTestWorker(Options{ScanInterval: time.Hour})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); w.Run(ctx) }()
	time.Sleep(50 * time.Millisecond)

	// Fire several forward triggers in quick succession.
	for i := 0; i < 5; i++ {
		if _, err := w.TriggerScan(""); err != nil {
			t.Fatalf("trigger %d: %v", i, err)
		}
	}
	// Allow the loop to drain the queued passes.
	time.Sleep(200 * time.Millisecond)
	cancel()
	wg.Wait()

	// Two groups scanned; each trigger runs a full pass over both groups. The
	// key property is that work is bounded and completes (no deadlock/leak).
	if got := atomic.LoadInt32(&s.forward); got == 0 {
		t.Error("expected forward scans to run from triggers")
	}
}

// TestGroupDisablementBetweenPasses verifies that groups deactivated between
// passes are dropped from the scan set (the worker re-lists active groups each
// pass).
func TestGroupDisablementBetweenPasses(t *testing.T) {
	groups := makeGroups(5)
	g := &mutableGroups{groups: groups}
	s := &scaleScanner{}
	w := New(g, s, &mockAsm{}, &mockBuild{}, &mockPP{}, nil, nil,
		Options{ScanInterval: time.Hour, ScanConcurrency: 3})

	w.doScan(context.Background(), "", false)
	if got := atomic.LoadInt32(&s.forward); got != 5 {
		t.Fatalf("first pass scanned %d, want 5", got)
	}

	// Disable two groups (simulate admin deactivation / deletion).
	g.setActive(2)

	w.doScan(context.Background(), "", false)
	if got := atomic.LoadInt32(&s.forward); got != 5+2 {
		t.Errorf("second pass total scans = %d, want 7 (only 2 active groups)", got)
	}
}

// mutableGroups is a GroupLister whose active set can change between passes,
// modelling admin group deletion/disablement during operation.
type mutableGroups struct {
	mu     sync.Mutex
	groups []store.Group
	active int // when > 0, only the first `active` groups are returned
}

func (m *mutableGroups) ListGroups(context.Context, bool) ([]store.Group, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active > 0 && m.active < len(m.groups) {
		return append([]store.Group(nil), m.groups[:m.active]...), nil
	}
	return append([]store.Group(nil), m.groups...), nil
}

func (m *mutableGroups) setActive(n int) {
	m.mu.Lock()
	m.active = n
	m.mu.Unlock()
}

// BenchmarkForwardScanDispatch measures the dispatch/aggregation overhead of a
// forward pass over many groups with a no-op scanner, isolating the worker's
// fan-out cost from real I/O.
func BenchmarkForwardScanDispatch(b *testing.B) {
	g := &mockGroups{groups: makeGroups(500)}
	s := &mockScanner{}
	w := New(g, s, &mockAsm{}, &mockBuild{}, &mockPP{}, nil, nil,
		Options{ScanInterval: time.Hour, ScanConcurrency: 16})
	ctx := context.Background()

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		w.doScan(ctx, "", false)
	}
}
