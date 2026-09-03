package worker

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vindicare/goindex/internal/assembler"
	"github.com/vindicare/goindex/internal/postprocess"
	"github.com/vindicare/goindex/internal/release"
	"github.com/vindicare/goindex/internal/scanner"
	"github.com/vindicare/goindex/internal/store"
)

type mockGroups struct{ groups []store.Group }

func (m *mockGroups) ListGroups(context.Context, bool) ([]store.Group, error) {
	return m.groups, nil
}

type mockScanner struct {
	forward  int32
	backfill int32
}

func (m *mockScanner) ScanForward(context.Context, string) (scanner.ScanResult, error) {
	atomic.AddInt32(&m.forward, 1)
	return scanner.ScanResult{ArticlesPulled: 10, PartsInserted: 8}, nil
}
func (m *mockScanner) ScanBackfill(context.Context, string) (scanner.ScanResult, error) {
	atomic.AddInt32(&m.backfill, 1)
	return scanner.ScanResult{ArticlesPulled: 5, PartsInserted: 4}, nil
}

type mockAsm struct{ calls int32 }

func (m *mockAsm) Assemble(context.Context) (assembler.Result, error) {
	atomic.AddInt32(&m.calls, 1)
	return assembler.Result{BinariesTouched: 3}, nil
}

type mockBuild struct{ calls int32 }

func (m *mockBuild) Build(context.Context) (release.Result, error) {
	atomic.AddInt32(&m.calls, 1)
	return release.Result{Created: 2}, nil
}

type mockPP struct{ calls int32 }

func (m *mockPP) Run(context.Context) (postprocess.Result, error) {
	atomic.AddInt32(&m.calls, 1)
	return postprocess.Result{Renamed: 1, NFOFound: 1}, nil
}

func newTestWorker(opts Options) (*Worker, *mockScanner, *mockAsm, *mockBuild, *mockPP) {
	g := &mockGroups{groups: []store.Group{{Name: "g1"}, {Name: "g2"}}}
	s := &mockScanner{}
	a := &mockAsm{}
	b := &mockBuild{}
	p := &mockPP{}
	w := New(g, s, a, b, p, nil, opts)
	return w, s, a, b, p
}

func TestScanAndDownstreamUpdateMetrics(t *testing.T) {
	w, s, a, b, p := newTestWorker(Options{ScanInterval: time.Hour})
	ctx := context.Background()

	w.doScan(ctx, "", false)
	w.runDownstream(ctx)

	// Two groups scanned forward once each.
	if got := atomic.LoadInt32(&s.forward); got != 2 {
		t.Errorf("forward scans = %d, want 2", got)
	}
	if atomic.LoadInt32(&a.calls) != 1 || atomic.LoadInt32(&b.calls) != 1 || atomic.LoadInt32(&p.calls) != 1 {
		t.Errorf("downstream stages should each run once: asm=%d build=%d pp=%d", a.calls, b.calls, p.calls)
	}

	m := w.Status().(Metrics)
	if m.Cycles != 1 {
		t.Errorf("cycles = %d, want 1", m.Cycles)
	}
	if m.ArticlesPulled != 20 || m.PartsInserted != 16 {
		t.Errorf("scan metrics = pulled %d parts %d, want 20/16", m.ArticlesPulled, m.PartsInserted)
	}
	if m.ReleasesCreated != 2 || m.ReleasesRenamed != 1 || m.NFOsFound != 1 {
		t.Errorf("release metrics = %+v", m)
	}
	if m.CurrentStage != "idle" {
		t.Errorf("stage = %q, want idle after downstream", m.CurrentStage)
	}
}

func TestBackfillOptionRunsBackfill(t *testing.T) {
	w, s, _, _, _ := newTestWorker(Options{ScanInterval: time.Hour, EnableBackfill: true})
	w.doScan(context.Background(), "", true)
	if atomic.LoadInt32(&s.backfill) != 2 {
		t.Errorf("backfill scans = %d, want 2 (one per group)", s.backfill)
	}
}

// TestDownstreamRunsWhileScanBlocked is the core #15 guarantee: a slow scan
// must not prevent post-processing from running.
func TestDownstreamRunsWhileScanBlocked(t *testing.T) {
	g := &mockGroups{groups: []store.Group{{Name: "g1"}}}
	scanStarted := make(chan struct{})
	releaseScan := make(chan struct{})
	s := &blockingScanner{started: scanStarted, release: releaseScan}
	a := &mockAsm{}
	b := &mockBuild{}
	p := &mockPP{}
	// Short intervals so both loops tick quickly.
	w := New(g, s, a, b, p, nil, Options{ScanInterval: 10 * time.Millisecond, DownstreamInterval: 10 * time.Millisecond})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)

	// Wait until the scan is in progress and blocked.
	select {
	case <-scanStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("scan did not start")
	}

	// While the scan is blocked, the downstream loop must still run post-process.
	deadline := time.After(2 * time.Second)
	for atomic.LoadInt32(&p.calls) == 0 {
		select {
		case <-deadline:
			t.Fatal("post-processing did not run while scan was blocked (starvation)")
		case <-time.After(10 * time.Millisecond):
		}
	}

	close(releaseScan) // let the scan finish
}

// blockingScanner blocks in ScanForward until released, to simulate a slow scan.
type blockingScanner struct {
	started   chan struct{}
	release   chan struct{}
	startOnce sync.Once
}

func (bs *blockingScanner) ScanForward(ctx context.Context, _ string) (scanner.ScanResult, error) {
	bs.startOnce.Do(func() { close(bs.started) })
	select {
	case <-bs.release:
	case <-ctx.Done():
	}
	return scanner.ScanResult{}, nil
}
func (bs *blockingScanner) ScanBackfill(context.Context, string) (scanner.ScanResult, error) {
	return scanner.ScanResult{}, nil
}

func TestTriggersAndRunLoopShutdown(t *testing.T) {
	w, s, _, _, _ := newTestWorker(Options{ScanInterval: time.Hour})

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		w.Run(ctx) // runs one initial cycle, then waits
	}()

	// Give the initial cycle time to run.
	time.Sleep(50 * time.Millisecond)
	initialForward := atomic.LoadInt32(&s.forward)
	if initialForward < 2 {
		t.Errorf("expected initial cycle to scan groups, got %d", initialForward)
	}

	// Fire a manual scan trigger for a single group.
	if err := w.TriggerScan("only-group"); err != nil {
		t.Fatalf("trigger scan: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if atomic.LoadInt32(&s.forward) <= initialForward {
		t.Error("manual scan trigger did not run")
	}

	// Backfill trigger.
	if err := w.TriggerBackfill("only-group"); err != nil {
		t.Fatalf("trigger backfill: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if atomic.LoadInt32(&s.backfill) < 1 {
		t.Error("manual backfill trigger did not run")
	}

	// Cancel and ensure Run returns (graceful shutdown).
	cancel()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not shut down within 2s of ctx cancel")
	}
}

func TestTriggerQueueFull(t *testing.T) {
	w, _, _, _, _ := newTestWorker(Options{ScanInterval: time.Hour})
	// Fill the trigger buffer (capacity 16) without a running loop to drain it.
	var lastErr error
	for i := 0; i < 20; i++ {
		lastErr = w.TriggerScan("g")
	}
	if lastErr == nil {
		t.Error("expected a 'queue full' error once buffer is saturated")
	}
}
