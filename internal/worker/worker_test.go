package worker

import (
	"context"
	"fmt"
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
	w := New(g, s, a, b, p, nil, nil, opts)
	return w, s, a, b, p
}

func TestScanAndDownstreamUpdateMetrics(t *testing.T) {
	w, s, a, b, p := newTestWorker(Options{ScanInterval: time.Hour})
	ctx := context.Background()

	w.doScan(ctx, "", false)
	w.runAssemble(ctx)
	w.runBuild(ctx)
	w.runPostProcess(ctx)

	// Two groups scanned forward once each.
	if got := atomic.LoadInt32(&s.forward); got != 2 {
		t.Errorf("forward scans = %d, want 2", got)
	}
	if atomic.LoadInt32(&a.calls) != 1 || atomic.LoadInt32(&b.calls) != 1 || atomic.LoadInt32(&p.calls) != 1 {
		t.Errorf("downstream stages should each run once: asm=%d build=%d pp=%d", a.calls, b.calls, p.calls)
	}

	m := w.Status().(Metrics)
	if m.Cycles != 1 {
		t.Errorf("cycles = %d, want 1 (one completed post-process pass)", m.Cycles)
	}
	if m.ArticlesPulled != 20 || m.PartsInserted != 16 {
		t.Errorf("scan metrics = pulled %d parts %d, want 20/16", m.ArticlesPulled, m.PartsInserted)
	}
	if m.ReleasesCreated != 2 || m.ReleasesRenamed != 1 || m.NFOsFound != 1 {
		t.Errorf("release metrics = %+v", m)
	}
	if m.CurrentStage != "idle" {
		t.Errorf("stage = %q, want idle after post-process", m.CurrentStage)
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
	// Short intervals so all loops tick quickly.
	w := New(g, s, a, b, p, nil, nil, Options{
		ScanInterval:        10 * time.Millisecond,
		DownstreamInterval:  10 * time.Millisecond,
		PostProcessInterval: 10 * time.Millisecond,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)

	// Wait until the scan is in progress and blocked.
	select {
	case <-scanStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("scan did not start")
	}

	// While the scan is blocked, the post-process loop must still run.
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

// TestPostProcessRunsWhileAssembleBlocked is the #15 follow-up guarantee: a
// large/slow assemble backlog must not prevent post-processing (name recovery)
// from running. This is the gap the live test exposed.
func TestPostProcessRunsWhileAssembleBlocked(t *testing.T) {
	g := &mockGroups{groups: []store.Group{{Name: "g1"}}}
	s := &mockScanner{}
	asmStarted := make(chan struct{})
	releaseAsm := make(chan struct{})
	a := &blockingAsm{started: asmStarted, release: releaseAsm}
	b := &mockBuild{}
	p := &mockPP{}
	w := New(g, s, a, b, p, nil, nil, Options{
		ScanInterval:        time.Hour, // keep scan out of the way
		DownstreamInterval:  10 * time.Millisecond,
		PostProcessInterval: 10 * time.Millisecond,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)

	// Wait until assemble is in progress and blocked.
	select {
	case <-asmStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("assemble did not start")
	}

	// While assemble is blocked, post-processing must still run.
	deadline := time.After(2 * time.Second)
	for atomic.LoadInt32(&p.calls) == 0 {
		select {
		case <-deadline:
			t.Fatal("post-processing did not run while assemble was blocked (starvation)")
		case <-time.After(10 * time.Millisecond):
		}
	}

	close(releaseAsm) // let assemble finish
}

// TestBuildRunsWhileAssembleBlocked verifies that release-building is not
// starved by a slow/long assemble pass (the gap found via live testing: a huge
// parts backlog kept the assemble loop busy so complete binaries never became
// releases).
func TestBuildRunsWhileAssembleBlocked(t *testing.T) {
	g := &mockGroups{groups: []store.Group{{Name: "g1"}}}
	s := &mockScanner{}
	asmStarted := make(chan struct{})
	releaseAsm := make(chan struct{})
	a := &blockingAsm{started: asmStarted, release: releaseAsm}
	b := &mockBuild{}
	p := &mockPP{}
	w := New(g, s, a, b, p, nil, nil, Options{
		ScanInterval:        time.Hour,
		DownstreamInterval:  10 * time.Millisecond,
		BuildInterval:       10 * time.Millisecond,
		PostProcessInterval: time.Hour,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)

	select {
	case <-asmStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("assemble did not start")
	}

	// While assemble is blocked, the build loop must still promote releases.
	deadline := time.After(2 * time.Second)
	for atomic.LoadInt32(&b.calls) == 0 {
		select {
		case <-deadline:
			t.Fatal("release-build did not run while assemble was blocked (starvation)")
		case <-time.After(10 * time.Millisecond):
		}
	}

	close(releaseAsm)
}

// blockingAsm blocks in Assemble until released, to simulate a slow assemble.
type blockingAsm struct {
	started   chan struct{}
	release   chan struct{}
	startOnce sync.Once
}

func (ba *blockingAsm) Assemble(ctx context.Context) (assembler.Result, error) {
	ba.startOnce.Do(func() { close(ba.started) })
	select {
	case <-ba.release:
	case <-ctx.Done():
	}
	return assembler.Result{}, nil
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
	if _, err := w.TriggerScan("only-group"); err != nil {
		t.Fatalf("trigger scan: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if atomic.LoadInt32(&s.forward) <= initialForward {
		t.Error("manual scan trigger did not run")
	}

	// Backfill trigger.
	if _, err := w.TriggerBackfill("only-group"); err != nil {
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
		_, lastErr = w.TriggerScan("g")
	}
	if lastErr == nil {
		t.Error("expected a 'queue full' error once buffer is saturated")
	}
}

func TestReconfigureUpdatesSchedule(t *testing.T) {
	w, _, _, _, _ := newTestWorker(Options{
		ScanInterval:        time.Hour,
		DownstreamInterval:  time.Hour,
		BuildInterval:       time.Hour,
		PostProcessInterval: time.Hour,
	})

	// A zero/negative field leaves the existing value unchanged.
	w.Reconfigure(Schedule{ScanInterval: 5 * time.Minute, BuildInterval: 0})
	got := w.CurrentSchedule()
	if got.ScanInterval != 5*time.Minute {
		t.Errorf("ScanInterval = %s, want 5m", got.ScanInterval)
	}
	if got.BuildInterval != time.Hour {
		t.Errorf("BuildInterval = %s, want unchanged 1h", got.BuildInterval)
	}
}

// TestReconfigureAppliesLive verifies a running loop adopts a shorter interval
// without a restart: with the post-process loop initially on a long interval,
// reconfiguring to a short one should drive additional passes.
func TestReconfigureAppliesLive(t *testing.T) {
	g := &mockGroups{groups: []store.Group{{Name: "g1"}}}
	s := &mockScanner{}
	a := &mockAsm{}
	b := &mockBuild{}
	p := &mockPP{}
	w := New(g, s, a, b, p, nil, nil, Options{
		ScanInterval:        time.Hour,
		DownstreamInterval:  time.Hour,
		BuildInterval:       time.Hour,
		PostProcessInterval: time.Hour, // effectively never on its own
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)

	// Wait for the initial post-process pass (each loop runs once at startup).
	waitForAtLeast(t, &p.calls, 1)

	// Now shorten the post-process interval; the loop should reset its ticker
	// and fire repeatedly.
	w.Reconfigure(Schedule{PostProcessInterval: 10 * time.Millisecond})
	waitForAtLeast(t, &p.calls, 4)
}

// waitForAtLeast polls an atomic counter until it reaches n or times out.
func waitForAtLeast(t *testing.T, counter *int32, n int32) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(counter) >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("counter reached %d, want >= %d", atomic.LoadInt32(counter), n)
}

// blockingPP blocks in Run until released, to simulate a slow post-process.
type blockingPP struct {
	started   chan struct{}
	release   chan struct{}
	startOnce sync.Once
}

func (bp *blockingPP) Run(ctx context.Context) (postprocess.Result, error) {
	bp.startOnce.Do(func() { close(bp.started) })
	select {
	case <-bp.release:
	case <-ctx.Done():
	}
	return postprocess.Result{}, nil
}

// TestActiveStagesConcurrent verifies that ActiveStages reflects every stage
// running at once, not just the last one set. A slow assemble and a slow
// post-process run concurrently, so both must appear.
func TestActiveStagesConcurrent(t *testing.T) {
	g := &mockGroups{groups: []store.Group{{Name: "g1"}}}
	s := &mockScanner{}
	asmStarted, releaseAsm := make(chan struct{}), make(chan struct{})
	ppStarted, releasePP := make(chan struct{}), make(chan struct{})
	a := &blockingAsm{started: asmStarted, release: releaseAsm}
	b := &mockBuild{}
	p := &blockingPP{started: ppStarted, release: releasePP}

	w := New(g, s, a, b, p, nil, nil, Options{
		ScanInterval:        time.Hour, // keep scan out of the way
		DownstreamInterval:  time.Hour,
		BuildInterval:       time.Hour,
		PostProcessInterval: time.Hour,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)

	// Both the assemble and post-process loops run an initial pass at startup
	// and will block inside it.
	<-asmStarted
	<-ppStarted

	m := w.MetricsSnapshot()
	got := map[string]bool{}
	for _, s := range m.ActiveStages {
		got[s] = true
	}
	if !got["assemble"] || !got["postprocess"] {
		t.Errorf("ActiveStages = %v, want both assemble and postprocess", m.ActiveStages)
	}

	// Release both; the stages should clear.
	close(releaseAsm)
	close(releasePP)
	waitForNoActiveStages(t, w)
}

func waitForNoActiveStages(t *testing.T, w *Worker) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(w.MetricsSnapshot().ActiveStages) == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Errorf("active stages did not clear: %v", w.MetricsSnapshot().ActiveStages)
}

// TestScanProgressReported verifies that while a scan is in flight, status
// reports the current group and its position in the group list, and that
// progress clears once the scan finishes.
func TestScanProgressReported(t *testing.T) {
	g := &mockGroups{groups: []store.Group{{Name: "g1"}, {Name: "g2"}, {Name: "g3"}}}
	started, release := make(chan struct{}), make(chan struct{})
	s := &blockingScanner{started: started, release: release}
	a := &mockAsm{}
	b := &mockBuild{}
	p := &mockPP{}
	w := New(g, s, a, b, p, nil, nil, Options{ScanInterval: time.Hour})

	done := make(chan struct{})
	go func() { w.doScan(context.Background(), "", false); close(done) }()

	// The scanner blocks inside the first group's ScanForward (concurrency 1).
	<-started
	sp := w.MetricsSnapshot().ScanProgress
	if sp == nil {
		t.Fatal("expected scan progress while scanning")
	}
	if len(sp.InFlight) != 1 || sp.InFlight[0] != "g1" || sp.Total != 3 || sp.Backfill {
		t.Errorf("scan progress = %+v, want in_flight=[g1] total=3 backfill=false", sp)
	}

	// Let the (single blocking) forward scan proceed to completion.
	close(release)
	<-done
	if sp := w.MetricsSnapshot().ScanProgress; sp != nil {
		t.Errorf("scan progress should be cleared after the pass, got %+v", sp)
	}
}

// gateScanner blocks every ScanForward until released, and records the peak
// number of concurrent in-flight scans, to verify parallel group scanning.
type gateScanner struct {
	release chan struct{}
	mu      sync.Mutex
	inFlight, peak int
	forward  int32
}

func (gs *gateScanner) enter() {
	gs.mu.Lock()
	gs.inFlight++
	if gs.inFlight > gs.peak {
		gs.peak = gs.inFlight
	}
	gs.mu.Unlock()
}
func (gs *gateScanner) leave() {
	gs.mu.Lock()
	gs.inFlight--
	gs.mu.Unlock()
}
func (gs *gateScanner) ScanForward(ctx context.Context, _ string) (scanner.ScanResult, error) {
	atomic.AddInt32(&gs.forward, 1)
	gs.enter()
	defer gs.leave()
	select {
	case <-gs.release:
	case <-ctx.Done():
	}
	return scanner.ScanResult{ArticlesPulled: 1, PartsInserted: 1}, nil
}
func (gs *gateScanner) ScanBackfill(context.Context, string) (scanner.ScanResult, error) {
	return scanner.ScanResult{}, nil
}

// TestParallelScanConcurrency verifies groups scan in parallel up to
// ScanConcurrency, that all groups are scanned exactly once, and that progress
// reports multiple in-flight groups.
func TestParallelScanConcurrency(t *testing.T) {
	groups := []store.Group{}
	for i := 0; i < 6; i++ {
		groups = append(groups, store.Group{Name: fmt.Sprintf("g%d", i)})
	}
	g := &mockGroups{groups: groups}
	release := make(chan struct{})
	s := &gateScanner{release: release}
	w := New(g, s, &mockAsm{}, &mockBuild{}, &mockPP{}, nil, nil,
		Options{ScanInterval: time.Hour, ScanConcurrency: 3})

	done := make(chan struct{})
	go func() { w.doScan(context.Background(), "", false); close(done) }()

	// Wait until 3 groups are concurrently in flight (bounded by concurrency).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		sp := w.MetricsSnapshot().ScanProgress
		if sp != nil && len(sp.InFlight) == 3 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	sp := w.MetricsSnapshot().ScanProgress
	if sp == nil || len(sp.InFlight) != 3 {
		t.Fatalf("expected 3 in-flight groups, got %+v", sp)
	}
	if sp.Total != 6 {
		t.Errorf("total = %d, want 6", sp.Total)
	}

	close(release)
	<-done

	// All 6 groups scanned exactly once, and peak concurrency never exceeded 3.
	if got := atomic.LoadInt32(&s.forward); got != 6 {
		t.Errorf("forward scans = %d, want 6 (each group once)", got)
	}
	s.mu.Lock()
	peak := s.peak
	s.mu.Unlock()
	if peak != 3 {
		t.Errorf("peak concurrency = %d, want 3 (bounded by ScanConcurrency)", peak)
	}
	if p := w.MetricsSnapshot().ScanProgress; p != nil {
		t.Errorf("scan progress should clear after pass, got %+v", p)
	}
	m := w.MetricsSnapshot()
	if m.ArticlesPulled != 6 || m.PartsInserted != 6 {
		t.Errorf("aggregated metrics pulled=%d parts=%d, want 6/6", m.ArticlesPulled, m.PartsInserted)
	}
}

// --- #113 job tracking tests ---

// fakeJobStore is an in-memory JobStore for verifying that manual triggers are
// recorded as persistent jobs with a lifecycle (queued -> running -> terminal).
type fakeJobStore struct {
	mu   sync.Mutex
	jobs map[string]*store.Job
}

func newFakeJobStore() *fakeJobStore {
	return &fakeJobStore{jobs: map[string]*store.Job{}}
}

func (f *fakeJobStore) CreateJob(_ context.Context, id, jobType, target string) (store.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	j := &store.Job{ID: id, Type: jobType, Target: target, State: store.JobQueued}
	f.jobs[id] = j
	return *j, nil
}

func (f *fakeJobStore) StartJob(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if j, ok := f.jobs[id]; ok {
		j.State = store.JobRunning
	}
	return nil
}

func (f *fakeJobStore) FinishJob(_ context.Context, id, state, errMsg string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if j, ok := f.jobs[id]; ok {
		j.State = state
		j.Error = errMsg
	}
	return nil
}

func (f *fakeJobStore) IsJobCancelRequested(_ context.Context, id string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if j, ok := f.jobs[id]; ok {
		return j.CancelRequested, nil
	}
	return false, nil
}

func (f *fakeJobStore) RequestJobCancel(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if j, ok := f.jobs[id]; ok {
		j.CancelRequested = true
	}
	return nil
}

func (f *fakeJobStore) state(id string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if j, ok := f.jobs[id]; ok {
		return j.State
	}
	return ""
}

func (f *fakeJobStore) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.jobs)
}

// TestTriggerRecordsJobToCompletion verifies a manual scan trigger creates a
// tracked job that is driven to the completed terminal state once the running
// loop processes it.
func TestTriggerRecordsJobToCompletion(t *testing.T) {
	w, _, _, _, _ := newTestWorker(Options{ScanInterval: time.Hour})
	fjs := newFakeJobStore()
	w.SetJobStore(fjs)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); w.Run(ctx) }()
	time.Sleep(50 * time.Millisecond)

	jobID, err := w.TriggerScan("g1")
	if err != nil {
		t.Fatalf("trigger scan: %v", err)
	}
	if jobID == "" {
		t.Fatal("expected a non-empty job id from a tracked trigger")
	}

	// Wait for the job to reach a terminal completed state.
	deadline := time.After(2 * time.Second)
	for {
		if fjs.state(jobID) == store.JobCompleted {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("job %s did not complete, last state=%q", jobID, fjs.state(jobID))
		case <-time.After(10 * time.Millisecond):
		}
	}

	cancel()
	wg.Wait()
}

// TestCancelJobBeforeRun verifies that requesting cancellation before the job
// is dequeued resolves it to the cancelled terminal state (work skipped).
func TestCancelJobBeforeRun(t *testing.T) {
	// Start a loop, wait for the initial cycle to settle, then enqueue a
	// trigger and immediately request cancel before it is dequeued. The job
	// must resolve to cancelled with its work skipped.
	w, _, _, _, _ := newTestWorker(Options{ScanInterval: time.Hour})
	fjs := newFakeJobStore()
	w.SetJobStore(fjs)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); w.Run(ctx) }()
	time.Sleep(50 * time.Millisecond)

	jobID, err := w.TriggerScan("g1")
	if err != nil {
		t.Fatalf("trigger scan: %v", err)
	}
	// Request cancellation right away; withJob checks the flag before starting.
	if err := w.CancelJob(jobID); err != nil {
		t.Fatalf("cancel job: %v", err)
	}

	deadline := time.After(2 * time.Second)
	for {
		if fjs.state(jobID) == store.JobCancelled {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("job %s not cancelled, last state=%q", jobID, fjs.state(jobID))
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	wg.Wait()
}

// TestTriggerQueueFullFailsJob verifies a trigger rejected due to a full queue
// records its job as failed rather than leaving it queued forever.
func TestTriggerQueueFullFailsJob(t *testing.T) {
	w, _, _, _, _ := newTestWorker(Options{ScanInterval: time.Hour})
	fjs := newFakeJobStore()
	w.SetJobStore(fjs)

	// No loop drains the queue (capacity 16). Saturate it.
	var lastID string
	var lastErr error
	for i := 0; i < 20; i++ {
		lastID, lastErr = w.TriggerScan("g")
	}
	if lastErr == nil {
		t.Fatal("expected a queue-full error after saturating the buffer")
	}
	if lastID != "" {
		t.Errorf("rejected trigger should return an empty id, got %q", lastID)
	}
	// Every created job should exist; the rejected ones are marked failed.
	if fjs.count() == 0 {
		t.Fatal("expected jobs to be recorded")
	}
	var failed int
	fjs.mu.Lock()
	for _, j := range fjs.jobs {
		if j.State == store.JobFailed {
			failed++
		}
	}
	fjs.mu.Unlock()
	if failed == 0 {
		t.Error("expected at least one job marked failed due to full queue")
	}
}

// --- #114 per-group scan recording test ---

// fakeScanRecorder records per-group scan outcomes for assertion.
type fakeScanRecorder struct {
	mu      sync.Mutex
	byGroup map[int64]store.GroupScanOutcome
	calls   int
}

func newFakeScanRecorder() *fakeScanRecorder {
	return &fakeScanRecorder{byGroup: map[int64]store.GroupScanOutcome{}}
}

func (f *fakeScanRecorder) RecordGroupScan(_ context.Context, id int64, o store.GroupScanOutcome) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byGroup[id] = o
	f.calls++
	return nil
}

func (f *fakeScanRecorder) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// idScanner is a scanner that reports a ServerHigh and records which groups it
// scanned forward, so we can assert per-group recording captures the outcome.
type idScanner struct {
	serverHigh int64
}

func (s *idScanner) ScanForward(_ context.Context, _ string) (scanner.ScanResult, error) {
	return scanner.ScanResult{ArticlesPulled: 7, PartsInserted: 6, ServerHigh: s.serverHigh}, nil
}
func (s *idScanner) ScanBackfill(_ context.Context, _ string) (scanner.ScanResult, error) {
	return scanner.ScanResult{ArticlesPulled: 3, PartsInserted: 2, ServerHigh: s.serverHigh}, nil
}

// idGroups lists groups with explicit IDs so the recorder can key by id.
type idGroups struct{ groups []store.Group }

func (m *idGroups) ListGroups(context.Context, bool) ([]store.Group, error) { return m.groups, nil }

func TestRunScanRecordsPerGroupOutcome(t *testing.T) {
	g := &idGroups{groups: []store.Group{{ID: 11, Name: "g-a"}, {ID: 22, Name: "g-b"}}}
	s := &idScanner{serverHigh: 9000}
	w := New(g, s, &mockAsm{}, &mockBuild{}, &mockPP{}, nil, nil, Options{ScanInterval: time.Hour})
	rec := newFakeScanRecorder()
	w.SetGroupScanRecorder(rec)

	w.doScan(context.Background(), "", false)

	if rec.count() != 2 {
		t.Fatalf("recorder calls = %d, want 2 (one per group)", rec.count())
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	a := rec.byGroup[11]
	if a.Articles != 7 || a.Parts != 6 || a.ServerHigh != 9000 || a.Backfill || a.Err != "" {
		t.Errorf("group 11 outcome = %+v, want articles 7 parts 6 head 9000 forward no-err", a)
	}
	if _, ok := rec.byGroup[22]; !ok {
		t.Error("expected group 22 to be recorded")
	}
}
