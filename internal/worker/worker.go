// Package worker orchestrates the goindex pipeline stages (scan -> assemble ->
// release -> post-process) as scheduled background jobs, with manual triggers,
// graceful shutdown, and status/metrics reporting.
package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/vindicare/goindex/internal/assembler"
	"github.com/vindicare/goindex/internal/notify"
	"github.com/vindicare/goindex/internal/postprocess"
	"github.com/vindicare/goindex/internal/release"
	"github.com/vindicare/goindex/internal/scanner"
	"github.com/vindicare/goindex/internal/store"
)

// GroupLister lists the active groups to scan.
type GroupLister interface {
	ListGroups(ctx context.Context, activeOnly bool) ([]store.Group, error)
}

// Scanner runs forward and backfill scans for a group.
type Scanner interface {
	ScanForward(ctx context.Context, group string) (scanner.ScanResult, error)
	ScanBackfill(ctx context.Context, group string) (scanner.ScanResult, error)
}

// Assembler folds parts into binaries.
type Assembler interface {
	Assemble(ctx context.Context) (assembler.Result, error)
}

// ReleaseBuilder promotes complete binaries into releases.
type ReleaseBuilder interface {
	Build(ctx context.Context) (release.Result, error)
}

// PostProcessor recovers names and NFO for pending releases.
type PostProcessor interface {
	Run(ctx context.Context) (postprocess.Result, error)
}

// Enricher matches releases to external metadata (TV show/movie). It is
// optional; a nil enricher (or one that reports Enabled()==false) disables the
// enrichment loop entirely.
type Enricher interface {
	Enabled() bool
	Run(ctx context.Context) error
}

// Options configures scheduling.
type Options struct {
	// ScanInterval is how often the scan loop runs (forward, plus backfill when
	// enabled).
	ScanInterval time.Duration
	// DownstreamInterval is how often the assemble loop runs, independently of
	// scanning so a long scan cannot starve it. Zero defaults to ScanInterval.
	DownstreamInterval time.Duration
	// BuildInterval is how often the release-build loop runs, independently of
	// assembly so a large parts backlog cannot starve release promotion. Zero
	// defaults to DownstreamInterval.
	BuildInterval time.Duration
	// PostProcessInterval is how often the post-process loop runs. It runs on
	// its own goroutine, independent of both scanning and assemble/build, so a
	// large parts backlog (slow assemble) cannot starve name recovery. Zero
	// defaults to DownstreamInterval.
	PostProcessInterval time.Duration
	// EnableBackfill runs a backfill pass alongside forward scans.
	EnableBackfill bool
	// EnrichInterval is how often the metadata-enrichment loop runs. Zero uses
	// a default. The loop only runs when an Enricher is supplied and enabled.
	EnrichInterval time.Duration
	// ScanConcurrency is how many groups a scan/backfill pass processes in
	// parallel. Zero or 1 means sequential (original behaviour); higher fans
	// out across a bounded worker pool, still capped by the NNTP pool size.
	ScanConcurrency int
	// AdaptiveMinInterval enables backlog-aware scheduling for the downstream
	// loops (assemble/build/post-process) (#125). When > 0, a pass that reports
	// it likely has more work pending schedules the next pass after this short
	// "busy" interval instead of the full configured interval, so a backlog is
	// worked down quickly; once a pass finds nothing, the loop backs off to the
	// configured interval. Zero disables adaptation (fixed intervals).
	AdaptiveMinInterval time.Duration
}

// Metrics is a snapshot of pipeline activity, exposed via Status().
type Metrics struct {
	Running          bool       `json:"running"`
	LastCycleStart   *time.Time `json:"last_cycle_start,omitempty"`
	LastCycleEnd     *time.Time `json:"last_cycle_end,omitempty"`
	LastError        string     `json:"last_error,omitempty"`
	Cycles           int64      `json:"cycles"`
	ArticlesPulled   int64      `json:"articles_pulled"`
	PartsInserted    int64      `json:"parts_inserted"`
	BinariesTouched  int64      `json:"binaries_touched"`
	ReleasesCreated  int64      `json:"releases_created"`
	ReleasesRenamed  int64      `json:"releases_renamed"`
	NFOsFound        int64      `json:"nfos_found"`
	// CurrentStage is the most recently entered stage (single value, kept for
	// compatibility). Because loops run concurrently it can be overwritten;
	// prefer ActiveStages for an accurate view of what is running now.
	CurrentStage string `json:"current_stage"`
	// ActiveStages lists every pipeline stage currently executing, sorted. The
	// loops run on independent goroutines, so more than one can be active at
	// once (e.g. "backfill" and "postprocess"). Empty means idle.
	ActiveStages []string `json:"active_stages"`
	// ScanProgress reports the current scan/backfill pass: which groups are
	// in flight (up to the scan concurrency) and how many of the pass's groups
	// have finished. Nil when no scan is running.
	ScanProgress *ScanProgress `json:"scan_progress,omitempty"`
}

// ScanProgress is a snapshot of the current scan/backfill pass across a bounded
// worker pool. Groups scan in parallel, so InFlight can hold several at once.
type ScanProgress struct {
	// InFlight lists the groups currently being scanned (sorted, up to the scan
	// concurrency).
	InFlight []string `json:"in_flight"`
	// Completed is how many of the pass's groups have finished; Total is the
	// number of groups in the pass.
	Completed int `json:"completed"`
	Total     int `json:"total"`
	// Backfill is true when this is a backfill pass (vs a forward scan).
	Backfill bool `json:"backfill"`
}

// Worker runs and coordinates the pipeline. The scan loop and the downstream
// (assemble -> build -> post-process) loop run as independent goroutines so a
// long-running scan cannot starve post-processing.
type Worker struct {
	groups  GroupLister
	scan    Scanner
	asm     Assembler
	build   ReleaseBuilder
	pp      PostProcessor
	enrich  Enricher
	log     *slog.Logger
	opts    Options

	// Manual job requests are delivered into their respective loops.
	scanTriggers chan scanTrigger
	ppTriggers   chan string // carries the job id (empty when untracked)

	// forwardDueFlag is set whenever a forward pass becomes due (ticker fired or
	// a global forward trigger arrived) while the scan loop is busy doing
	// backfill, so backfill yields to it promptly (#112). Guarded atomically so
	// the watcher can set it without contending on scanMu.
	forwardDueFlag atomic.Bool

	// Persistent jobs (#113). jobs records job lifecycle (nil disables it).
	// jobCancels maps an in-flight manual job id to a cancel func so CancelJob
	// can cooperatively stop it; guarded by jobMu.
	jobs       JobStore
	jobMu      sync.Mutex
	jobCancels map[string]context.CancelFunc
	// runCtx is the worker's run context, used as the parent for per-job
	// cancellable contexts. Set in Run.
	runCtx context.Context

	// optsMu guards the schedule intervals in opts, which Reconfigure updates
	// at runtime. The loops read intervals through the accessor methods.
	optsMu sync.RWMutex
	// Per-loop reset signals: Reconfigure pokes these so each loop rebuilds its
	// ticker with the new interval without a restart. Buffered (cap 1) so a
	// reconfigure never blocks and coalesces with a pending reset.
	scanReset  chan struct{}
	asmReset   chan struct{}
	buildReset chan struct{}
	ppReset    chan struct{}

	// Each loop has its own mutex so scan, assemble/build, and post-process run
	// concurrently while each is serialised internally.
	scanMu   sync.Mutex
	asmMu    sync.Mutex
	buildMu  sync.Mutex
	ppMu     sync.Mutex
	enrichMu sync.Mutex

	mu      sync.Mutex
	metrics Metrics
	// active is the set of pipeline stages currently executing (guarded by mu).
	// Loops run concurrently, so several may be active at once.
	active map[string]bool
	// Scan-pass progress (guarded by mu). scanActive indicates a pass is
	// running; scanInFlight is the set of groups currently being scanned;
	// scanCompleted/scanTotal track pass progress; scanBackfill records the
	// pass type. All zero/empty when no scan is running.
	scanActive    bool
	scanInFlight  map[string]bool
	scanCompleted int
	scanTotal     int
	scanBackfill  bool

	// scanRecorder persists per-group scan outcomes (#114). Optional: when nil,
	// per-group scan progress/error state is not recorded (aggregate metrics
	// still update). The store.Store satisfies it.
	scanRecorder GroupScanRecorder

	// notifier emits pipeline events to external webhooks (#137). Optional:
	// when nil, no notifications are sent. Emit is non-blocking and best-effort,
	// so it never affects pipeline timing.
	notifier Notifier

	// errHistory is a bounded ring buffer of recent pipeline errors (#133), so
	// concurrent worker failures do not overwrite one another the way a single
	// last-error field does. Guarded by errMu. In-memory only: the history
	// covers the current process lifetime (durable per-group and per-release
	// errors are retained in the database).
	errMu      sync.Mutex
	errHistory []PipelineError
	errSeq     int64
}

// PipelineError is one recorded pipeline failure retained in the worker's
// bounded in-memory history (#133). Messages are stage/error text only — no
// article contents or credentials.
type PipelineError struct {
	// Seq is a monotonically increasing id (newest highest), usable as an
	// acknowledge cursor.
	Seq int64 `json:"seq"`
	// Stage is the pipeline stage (scan/backfill/assemble/build/postprocess/
	// enrich), or "" when unknown.
	Stage string `json:"stage,omitempty"`
	// Group is the affected newsgroup when the failure is group-scoped.
	Group string `json:"group,omitempty"`
	// Message is the error text.
	Message string `json:"message"`
	// At is when the error was recorded.
	At time.Time `json:"at"`
}

// maxErrHistory bounds the in-memory pipeline-error ring (#133).
const maxErrHistory = 200

// Notifier receives pipeline events for external notification (#137). The
// notify.Service satisfies it. Emit must be non-blocking.
type Notifier interface {
	Emit(e notify.Event)
}

// SetNotifier attaches an event notifier used to publish job and error events
// to configured webhooks (#137). Optional.
func (w *Worker) SetNotifier(n Notifier) { w.notifier = n }

// emit publishes an event when a notifier is attached (best-effort).
func (w *Worker) emit(e notify.Event) {
	if w.notifier != nil {
		w.notifier.Emit(e)
	}
}

// GroupScanRecorder persists the outcome of the most recent scan/backfill pass
// for a group (#114). Optional; the store.Store satisfies it.
type GroupScanRecorder interface {
	RecordGroupScan(ctx context.Context, id int64, o store.GroupScanOutcome) error
}

// scanTrigger is a manual scan request. backfill selects a backfill pass.
// jobID, when set, is the persistent job to update as the pass runs (#113).
type scanTrigger struct {
	group    string // empty = all active groups
	backfill bool
	jobID    string
}

// JobStore persists pipeline job lifecycle (#113). Optional: when nil, triggers
// run without recording jobs (returning an empty id). The store.Store
// satisfies this.
type JobStore interface {
	CreateJob(ctx context.Context, id, jobType, target string) (store.Job, error)
	StartJob(ctx context.Context, id string) error
	FinishJob(ctx context.Context, id, state, errMsg string) error
	IsJobCancelRequested(ctx context.Context, id string) (bool, error)
	RequestJobCancel(ctx context.Context, id string) error
}

// New creates a Worker. enrich may be nil to disable metadata enrichment.
func New(groups GroupLister, scan Scanner, asm Assembler, build ReleaseBuilder, pp PostProcessor, enrich Enricher, log *slog.Logger, opts Options) *Worker {
	if opts.ScanInterval <= 0 {
		opts.ScanInterval = 15 * time.Minute
	}
	if opts.EnrichInterval <= 0 {
		opts.EnrichInterval = 30 * time.Minute
	}
	if opts.ScanConcurrency < 1 {
		opts.ScanConcurrency = 1
	}
	if opts.DownstreamInterval <= 0 {
		opts.DownstreamInterval = opts.ScanInterval
	}
	if opts.BuildInterval <= 0 {
		opts.BuildInterval = opts.DownstreamInterval
	}
	if opts.PostProcessInterval <= 0 {
		opts.PostProcessInterval = opts.DownstreamInterval
	}
	if log == nil {
		log = slog.Default()
	}
	return &Worker{
		groups:       groups,
		scan:         scan,
		asm:          asm,
		build:        build,
		pp:           pp,
		enrich:       enrich,
		log:          log,
		opts:         opts,
		scanTriggers: make(chan scanTrigger, 16),
		ppTriggers:   make(chan string, 16),
		scanReset:    make(chan struct{}, 1),
		asmReset:     make(chan struct{}, 1),
		buildReset:   make(chan struct{}, 1),
		ppReset:      make(chan struct{}, 1),
		active:       map[string]bool{},
		jobCancels:   map[string]context.CancelFunc{},
	}
}

// SetJobStore attaches a job store so manual triggers are recorded as
// persistent jobs with IDs, progress, and cancellation (#113). Optional.
func (w *Worker) SetJobStore(js JobStore) { w.jobs = js }

// SetGroupScanRecorder attaches a recorder so each group's scan/backfill
// outcome (time, counts, observed server head, error) is persisted for
// per-group progress reporting (#114). Optional.
func (w *Worker) SetGroupScanRecorder(r GroupScanRecorder) { w.scanRecorder = r }

// newJobID returns a fresh job identifier.
func newJobID() string { return uuid.NewString() }

// scanInterval, downstreamInterval, buildInterval, and postProcessInterval
// return the current schedule intervals under the opts lock.
func (w *Worker) scanInterval() time.Duration {
	w.optsMu.RLock()
	defer w.optsMu.RUnlock()
	return w.opts.ScanInterval
}

func (w *Worker) downstreamInterval() time.Duration {
	w.optsMu.RLock()
	defer w.optsMu.RUnlock()
	return w.opts.DownstreamInterval
}

func (w *Worker) buildInterval() time.Duration {
	w.optsMu.RLock()
	defer w.optsMu.RUnlock()
	return w.opts.BuildInterval
}

func (w *Worker) postProcessInterval() time.Duration {
	w.optsMu.RLock()
	defer w.optsMu.RUnlock()
	return w.opts.PostProcessInterval
}

// adaptiveMinInterval returns the configured backlog-aware "busy" interval
// (#125). Zero means adaptation is disabled.
func (w *Worker) adaptiveMinInterval() time.Duration {
	w.optsMu.RLock()
	defer w.optsMu.RUnlock()
	return w.opts.AdaptiveMinInterval
}

// nextInterval computes the delay before a loop's next pass given whether the
// last pass reported more work likely pending (#125). When adaptation is on and
// the loop is busy, it uses the shorter of the busy interval and the configured
// interval; otherwise it uses the configured interval. This lets a backlog be
// worked down quickly while an idle pipeline runs at its normal cadence.
func (w *Worker) nextInterval(configured time.Duration, busy bool) time.Duration {
	minI := w.adaptiveMinInterval()
	if minI <= 0 || !busy {
		return configured
	}
	if minI < configured {
		return minI
	}
	return configured
}

// Schedule is the set of runtime-tunable pipeline intervals.
type Schedule struct {
	ScanInterval        time.Duration `json:"scan_interval"`
	DownstreamInterval  time.Duration `json:"downstream_interval"`
	BuildInterval       time.Duration `json:"build_interval"`
	PostProcessInterval time.Duration `json:"postprocess_interval"`
}

// CurrentSchedule returns the current schedule intervals.
func (w *Worker) CurrentSchedule() Schedule {
	w.optsMu.RLock()
	defer w.optsMu.RUnlock()
	return Schedule{
		ScanInterval:        w.opts.ScanInterval,
		DownstreamInterval:  w.opts.DownstreamInterval,
		BuildInterval:       w.opts.BuildInterval,
		PostProcessInterval: w.opts.PostProcessInterval,
	}
}

// Reconfigure updates the schedule intervals live. Any interval <= 0 is left
// unchanged. Each affected loop resets its ticker to the new cadence without a
// restart. Safe to call concurrently with the running loops.
func (w *Worker) Reconfigure(s Schedule) {
	w.optsMu.Lock()
	if s.ScanInterval > 0 {
		w.opts.ScanInterval = s.ScanInterval
	}
	if s.DownstreamInterval > 0 {
		w.opts.DownstreamInterval = s.DownstreamInterval
	}
	if s.BuildInterval > 0 {
		w.opts.BuildInterval = s.BuildInterval
	}
	if s.PostProcessInterval > 0 {
		w.opts.PostProcessInterval = s.PostProcessInterval
	}
	w.optsMu.Unlock()

	w.log.Info("worker schedule reconfigured",
		"scan_interval", w.scanInterval(),
		"downstream_interval", w.downstreamInterval(),
		"build_interval", w.buildInterval(),
		"postprocess_interval", w.postProcessInterval())

	// Poke each loop to reset its ticker. Non-blocking: a pending reset already
	// covers the change.
	for _, ch := range []chan struct{}{w.scanReset, w.asmReset, w.buildReset, w.ppReset} {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// Run drives the scan, assemble/build, and post-process loops as independent
// goroutines until ctx is cancelled, then blocks until all have stopped.
// Because the loops are independent, neither a long-running scan nor a large
// assemble backlog can starve post-processing (name recovery).
func (w *Worker) Run(ctx context.Context) {
	w.runCtx = ctx
	w.log.Info("worker started",
		"scan_interval", w.opts.ScanInterval,
		"downstream_interval", w.opts.DownstreamInterval,
		"build_interval", w.opts.BuildInterval,
		"postprocess_interval", w.opts.PostProcessInterval,
		"backfill", w.opts.EnableBackfill)

	var wg sync.WaitGroup
	wg.Add(4)
	go func() { defer wg.Done(); w.scanLoop(ctx) }()
	go func() { defer wg.Done(); w.assembleLoop(ctx) }()
	go func() { defer wg.Done(); w.buildLoop(ctx) }()
	go func() { defer wg.Done(); w.postProcessLoop(ctx) }()
	if w.enrich != nil && w.enrich.Enabled() {
		wg.Add(1)
		go func() { defer wg.Done(); w.enrichLoop(ctx) }()
	}
	wg.Wait()
	w.log.Info("worker stopped")
}

// scanLoop drives scanning with forward priority (#112): forward passes always
// run before backfill, and backfill runs one group at a time, yielding to any
// forward pass that becomes due. This ensures newly posted content is indexed
// promptly even during a large historical backfill, while backfill still makes
// progress when forward demand is low. Forward and backfill share this single
// goroutine, so no group is ever scanned forward and backward concurrently.
func (w *Worker) scanLoop(ctx context.Context) {
	ticker := time.NewTicker(w.scanInterval())
	defer ticker.Stop()

	sched := &scanScheduler{
		runner: workerScanRunner{w},
		// forwardDue reports whether a forward pass has become due while backfill
		// is running. It non-blockingly polls the ticker and forward triggers so
		// backfill (which holds this single goroutine) yields to forward
		// promptly, between groups.
		forwardDue:      func() bool { return w.forwardBecameDue(ticker) },
		backfillEnabled: func() bool { return w.opts.EnableBackfill },
	}

	// Initial forward pass first, then drain backfill (yielding to forward).
	sched.runForwardPass(ctx, "")
	w.drainBackfillYieldingToForward(ctx, sched)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sched.runForwardPass(ctx, "")
			w.drainBackfillYieldingToForward(ctx, sched)
		case t := <-w.scanTriggers:
			w.runTrackedScan(ctx, t, sched)
		case <-w.scanReset:
			ticker.Reset(w.scanInterval())
		}
	}
}

// forwardBecameDue non-blockingly checks whether a forward pass is now due: the
// scan ticker fired, or a global forward trigger is queued. When true it also
// records the fact in forwardDueFlag so the caller can service it. Group-scoped
// or backfill triggers are left on the queue for the main select to handle.
func (w *Worker) forwardBecameDue(ticker *time.Ticker) bool {
	if w.forwardDueFlag.Load() {
		return true
	}
	select {
	case <-ticker.C:
		w.forwardDueFlag.Store(true)
		return true
	default:
	}
	// Peek for a queued global forward trigger without discarding other work.
	select {
	case t := <-w.scanTriggers:
		if !t.backfill && t.group == "" {
			w.forwardDueFlag.Store(true)
			return true
		}
		// Not a global forward trigger: put it back for the main loop. The
		// buffered channel has capacity, so this does not block in practice.
		select {
		case w.scanTriggers <- t:
		default:
		}
	default:
	}
	return false
}

// drainBackfillYieldingToForward runs the backfill drain and, whenever it
// yields because a forward pass became due, services that forward pass and
// resumes backfill — so forward work is never starved and backfill still
// completes over time.
func (w *Worker) drainBackfillYieldingToForward(ctx context.Context, sched *scanScheduler) {
	for {
		if ctx.Err() != nil {
			return
		}
		yielded := sched.drainBackfill(ctx)
		if !yielded {
			return
		}
		// Forward became due mid-backfill: consume the pending signal and run
		// the forward pass, then resume backfill. This is the scheduling
		// decision that guarantees forward is never starved by backfill (#112).
		w.forwardDueFlag.Store(false)
		w.log.Debug("scan scheduler: yielding backfill to overdue forward pass")
		sched.runForwardPass(ctx, "")
	}
}

// assembleLoop runs an initial assemble pass, then on DownstreamInterval. It
// folds parts into binaries independently of scanning, building, and
// post-processing.
func (w *Worker) assembleLoop(ctx context.Context) {
	ticker := time.NewTicker(w.downstreamInterval())
	defer ticker.Stop()

	// After each pass, schedule the next one adaptively (#125): sooner while a
	// backlog remains, at the configured cadence once drained.
	busy := w.runAssemble(ctx)
	ticker.Reset(w.nextInterval(w.downstreamInterval(), busy))

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			busy = w.runAssemble(ctx)
			ticker.Reset(w.nextInterval(w.downstreamInterval(), busy))
		case <-w.asmReset:
			ticker.Reset(w.downstreamInterval())
		}
	}
}

// buildLoop promotes complete binaries into releases on BuildInterval,
// independently of assembly. This matters at scale: a large parts backlog can
// keep the assemble loop busy for a long time, but complete binaries must still
// become releases promptly instead of waiting for assembly to fully drain.
func (w *Worker) buildLoop(ctx context.Context) {
	ticker := time.NewTicker(w.buildInterval())
	defer ticker.Stop()

	busy := w.runBuild(ctx)
	ticker.Reset(w.nextInterval(w.buildInterval(), busy))

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			busy = w.runBuild(ctx)
			ticker.Reset(w.nextInterval(w.buildInterval(), busy))
		case <-w.buildReset:
			ticker.Reset(w.buildInterval())
		}
	}
}

// postProcessLoop runs an initial post-process pass, then on
// PostProcessInterval and on manual triggers. It runs independently of both
// scanning and assemble/build, so a large parts backlog cannot starve name
// recovery.
func (w *Worker) postProcessLoop(ctx context.Context) {
	ticker := time.NewTicker(w.postProcessInterval())
	defer ticker.Stop()

	busy := w.runPostProcess(ctx)
	ticker.Reset(w.nextInterval(w.postProcessInterval(), busy))

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			busy = w.runPostProcess(ctx)
			ticker.Reset(w.nextInterval(w.postProcessInterval(), busy))
		case jobID := <-w.ppTriggers:
			w.runTrackedPostProcess(ctx, jobID)
		case <-w.ppReset:
			ticker.Reset(w.postProcessInterval())
		}
	}
}

// enrichLoop runs an initial metadata-enrichment pass, then on EnrichInterval.
// It runs independently of the other loops so metadata lookups (which hit an
// external API) never block the pipeline.
func (w *Worker) enrichLoop(ctx context.Context) {
	ticker := time.NewTicker(w.opts.EnrichInterval)
	defer ticker.Stop()

	w.runEnrich(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.runEnrich(ctx)
		}
	}
}

// runEnrich runs one metadata-enrichment pass under its own mutex.
func (w *Worker) runEnrich(ctx context.Context) {
	w.enrichMu.Lock()
	defer w.enrichMu.Unlock()
	if ctx.Err() != nil {
		return
	}
	w.stageStart("enrich")
	defer w.stageEnd("enrich")
	if err := w.enrich.Run(ctx); err != nil {
		w.recordStageError("enrich", "", fmt.Errorf("metadata enrichment: %w", err))
	}
}

// doScan runs one scan pass under the scan mutex (so scans don't overlap).
func (w *Worker) doScan(ctx context.Context, group string, backfill bool) {
	w.scanMu.Lock()
	defer w.scanMu.Unlock()
	w.runScan(ctx, group, backfill)
}

// workerScanRunner adapts the Worker to the scanScheduler's scanRunner, giving
// forward priority over backfill (#112).
type workerScanRunner struct{ w *Worker }

func (r workerScanRunner) runForward(ctx context.Context, group string) {
	r.w.doScan(ctx, group, false)
}

// listBackfillGroups returns the names of the groups eligible for backfill.
func (r workerScanRunner) listBackfillGroups(ctx context.Context) []string {
	groups, err := r.w.groups.ListGroups(ctx, true)
	if err != nil {
		r.w.recordError(fmt.Errorf("list backfill groups: %w", err))
		return nil
	}
	names := make([]string, 0, len(groups))
	for _, g := range groups {
		names = append(names, g.Name)
	}
	return names
}

// scanBackfillGroup backfills exactly one group under the scan mutex, so it
// never overlaps a forward scan of the same (or any) group.
func (r workerScanRunner) scanBackfillGroup(ctx context.Context, group string) {
	r.w.doScan(ctx, group, true)
}

// runScan scans a single named group or all active groups.
func (w *Worker) runScan(ctx context.Context, group string, backfill bool) {
	stage := "scan"
	if backfill {
		stage = "backfill"
	}
	w.stageStart(stage)
	defer w.stageEnd(stage)

	groups, err := w.targetGroups(ctx, group)
	if err != nil {
		w.recordError(fmt.Errorf("list groups: %w", err))
		return
	}

	w.scanPassBegin(len(groups), backfill)
	defer w.scanPassEnd()

	// Bound the number of groups scanned in parallel. Each worker draws NNTP
	// connections from the shared pool, so real parallelism is additionally
	// capped by the pool size; workers block in acquire() rather than failing
	// when the pool is exhausted. A concurrency of 1 preserves the original
	// sequential behaviour. The whole pass runs under scanMu (see doScan), and
	// each group is dispatched to exactly one worker, so no group is scanned by
	// two goroutines at once (which would race on its watermark row).
	workers := w.opts.ScanConcurrency
	if workers < 1 {
		workers = 1
	}
	if workers > len(groups) {
		workers = len(groups)
	}
	if workers == 0 {
		return // no groups
	}

	jobs := make(chan store.Group)
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			for g := range jobs {
				if ctx.Err() != nil {
					return
				}
				w.scanGroupBegin(g.Name)
				var (
					res  scanner.ScanResult
					serr error
				)
				passStart := time.Now()
				if backfill {
					res, serr = w.scan.ScanBackfill(ctx, g.Name)
				} else {
					res, serr = w.scan.ScanForward(ctx, g.Name)
				}
				passDur := time.Since(passStart)
				if serr != nil {
					stage := "scan"
					if backfill {
						stage = "backfill"
					}
					w.recordStageError(stage, g.Name, fmt.Errorf("scan %s: %w", g.Name, serr))
				} else {
					w.mu.Lock()
					w.metrics.ArticlesPulled += res.ArticlesPulled
					w.metrics.PartsInserted += res.PartsInserted
					w.mu.Unlock()
				}
				w.recordGroupScan(ctx, g, res, serr, passDur)
				w.scanGroupEnd(g.Name)
			}
		}()
	}

	for _, g := range groups {
		select {
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return
		case jobs <- g:
		}
	}
	close(jobs)
	wg.Wait()
}

// runAssemble folds parts into binaries under the assemble mutex, so a
// scheduled pass and a manual trigger cannot overlap. It runs independently of
// building and post-processing.
// runAssemble returns whether the pass likely left more work pending (a backlog
// remains), so the loop can schedule the next pass sooner (#125).
func (w *Worker) runAssemble(ctx context.Context) (busy bool) {
	w.asmMu.Lock()
	defer w.asmMu.Unlock()

	if ctx.Err() != nil {
		return false
	}
	w.stageStart("assemble")
	defer w.stageEnd("assemble")
	asmRes, err := w.asm.Assemble(ctx)
	if err != nil {
		w.recordStageError("assemble", "", fmt.Errorf("assemble: %w", err))
		return false
	}
	w.mu.Lock()
	w.metrics.BinariesTouched += int64(asmRes.BinariesTouched)
	w.mu.Unlock()
	// A pass that did not fully drain the backlog has more to do.
	return !asmRes.Drained
}

// runBuild promotes complete binaries into releases under the build mutex. It
// runs independently of assembly, so complete binaries become releases
// promptly even while a large parts backlog keeps the assemble loop busy.
// runBuild returns whether the pass processed any binaries, a proxy for "more
// complete binaries may remain" so the loop can promote them promptly (#125).
func (w *Worker) runBuild(ctx context.Context) (busy bool) {
	w.buildMu.Lock()
	defer w.buildMu.Unlock()

	if ctx.Err() != nil {
		return false
	}
	w.stageStart("release")
	defer w.stageEnd("release")
	buildRes, err := w.build.Build(ctx)
	if err != nil {
		w.recordStageError("build", "", fmt.Errorf("build releases: %w", err))
		return false
	}
	w.mu.Lock()
	w.metrics.ReleasesCreated += int64(buildRes.Created)
	w.mu.Unlock()
	// If the pass promoted binaries, more complete ones may be waiting.
	return buildRes.Processed > 0
}

// runPostProcess runs one post-processing pass under the post-process mutex,
// independently of scanning and assemble/build. It records metrics and counts
// a completed pass as a cycle.
// runPostProcess returns whether the pass processed any releases, a proxy for
// "more pending releases may remain" so the loop can drain the queue promptly
// (#125).
func (w *Worker) runPostProcess(ctx context.Context) (busy bool) {
	w.ppMu.Lock()
	defer w.ppMu.Unlock()

	if ctx.Err() != nil {
		return false
	}
	start := time.Now()
	w.mu.Lock()
	w.metrics.Running = true
	w.metrics.LastCycleStart = &start
	w.mu.Unlock()
	w.stageStart("postprocess")
	defer func() {
		w.stageEnd("postprocess")
		w.mu.Lock()
		w.metrics.Running = false
		w.mu.Unlock()
	}()

	ppRes, err := w.pp.Run(ctx)
	if err != nil {
		w.recordStageError("postprocess", "", fmt.Errorf("post-process: %w", err))
		return false
	}
	w.mu.Lock()
	w.metrics.ReleasesRenamed += int64(ppRes.Renamed)
	w.metrics.NFOsFound += int64(ppRes.NFOFound)
	w.metrics.Cycles++
	end := time.Now()
	w.metrics.LastCycleEnd = &end
	w.mu.Unlock()
	// If the pass processed anything, more pending releases may remain.
	return ppRes.Processed > 0
}

// targetGroups resolves the group set for a scan: one named group or all active.
func (w *Worker) targetGroups(ctx context.Context, group string) ([]store.Group, error) {
	if group != "" {
		return []store.Group{{Name: group}}, nil
	}
	return w.groups.ListGroups(ctx, true)
}

// --- persistent job tracking (#113) ---

// createJob records a queued job and returns its id, or "" when no job store is
// attached (untracked operation).
func (w *Worker) createJob(jobType, target string) string {
	if w.jobs == nil {
		return ""
	}
	id := newJobID()
	if _, err := w.jobs.CreateJob(context.Background(), id, jobType, target); err != nil {
		w.log.Warn("failed to record job", "type", jobType, "err", err)
		return ""
	}
	return id
}

// failJobQueued marks a just-created job as failed when it couldn't be enqueued
// (queue full), so it doesn't linger as queued forever.
func (w *Worker) failJobQueued(jobID string) {
	if w.jobs == nil || jobID == "" {
		return
	}
	_ = w.jobs.FinishJob(context.Background(), jobID, store.JobFailed, "worker busy: trigger queue full")
}

// withJob runs fn under a per-job cancellable context derived from the worker
// run context, recording the job's running/terminal lifecycle. The job is
// marked cancelled when cancellation was requested, failed on error, else
// completed. When jobID is empty (untracked) it simply runs fn(ctx).
func (w *Worker) withJob(parent context.Context, jobID string, fn func(ctx context.Context) error) {
	if w.jobs == nil || jobID == "" {
		_ = fn(parent)
		return
	}

	jobCtx, cancel := context.WithCancel(parent)
	w.jobMu.Lock()
	w.jobCancels[jobID] = cancel
	w.jobMu.Unlock()
	defer func() {
		cancel()
		w.jobMu.Lock()
		delete(w.jobCancels, jobID)
		w.jobMu.Unlock()
	}()

	// If cancellation was already requested before we started, skip the work.
	if req, _ := w.jobs.IsJobCancelRequested(context.Background(), jobID); req {
		_ = w.jobs.FinishJob(context.Background(), jobID, store.JobCancelled, "")
		return
	}
	_ = w.jobs.StartJob(context.Background(), jobID)

	err := fn(jobCtx)

	// Determine terminal state. A cancel request (or a cancelled context)
	// resolves to cancelled; otherwise error->failed, success->completed.
	if req, _ := w.jobs.IsJobCancelRequested(context.Background(), jobID); req || jobCtx.Err() != nil {
		_ = w.jobs.FinishJob(context.Background(), jobID, store.JobCancelled, "")
		return
	}
	if err != nil {
		_ = w.jobs.FinishJob(context.Background(), jobID, store.JobFailed, err.Error())
		w.emit(notify.Event{
			Type:    notify.EventJobFailed,
			Title:   "Pipeline job failed",
			Message: err.Error(),
			Fields:  map[string]string{"job_id": jobID},
		})
		return
	}
	_ = w.jobs.FinishJob(context.Background(), jobID, store.JobCompleted, "")
	w.emit(notify.Event{
		Type:   notify.EventJobCompleted,
		Title:  "Pipeline job completed",
		Fields: map[string]string{"job_id": jobID},
	})
}

// runTrackedScan runs a manual scan/backfill trigger under job tracking.
func (w *Worker) runTrackedScan(parent context.Context, t scanTrigger, sched *scanScheduler) {
	w.withJob(parent, t.jobID, func(ctx context.Context) error {
		if t.backfill {
			if t.group != "" {
				w.doScan(ctx, t.group, true)
			} else {
				w.drainBackfillYieldingToForward(ctx, sched)
			}
		} else {
			w.doScan(ctx, t.group, false)
			w.drainBackfillYieldingToForward(ctx, sched)
		}
		return w.lastPassError()
	})
}

// runTrackedPostProcess runs a manual post-process trigger under job tracking.
func (w *Worker) runTrackedPostProcess(parent context.Context, jobID string) {
	w.withJob(parent, jobID, func(ctx context.Context) error {
		w.runPostProcess(ctx)
		return w.lastPassError()
	})
}

// lastPassError returns the most recent recorded pipeline error, if any, so a
// tracked job reflects failures surfaced via recordError. Returns nil when the
// last recorded error is empty.
func (w *Worker) lastPassError() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.metrics.LastError == "" {
		return nil
	}
	return errors.New(w.metrics.LastError)
}

// --- rest.JobController implementation ---

// TriggerScan requests a forward scan (non-blocking) and returns the persistent
// job id (#113). Empty id when no job store is attached.
func (w *Worker) TriggerScan(group string) (string, error) {
	jobID := w.createJob("scan", group)
	select {
	case w.scanTriggers <- scanTrigger{group: group, backfill: false, jobID: jobID}:
		return jobID, nil
	default:
		w.failJobQueued(jobID)
		return "", fmt.Errorf("worker busy: scan trigger queue full")
	}
}

// TriggerBackfill requests a backfill pass (non-blocking) and returns the
// persistent job id (#113).
func (w *Worker) TriggerBackfill(group string) (string, error) {
	jobID := w.createJob("backfill", group)
	select {
	case w.scanTriggers <- scanTrigger{group: group, backfill: true, jobID: jobID}:
		return jobID, nil
	default:
		w.failJobQueued(jobID)
		return "", fmt.Errorf("worker busy: scan trigger queue full")
	}
}

// TriggerPostProcess requests an immediate post-processing pass (non-blocking)
// and returns the persistent job id (#113). This lets an operator recover names
// for pending releases without waiting for a scan or the post-process interval.
func (w *Worker) TriggerPostProcess() (string, error) {
	jobID := w.createJob("postprocess", "")
	select {
	case w.ppTriggers <- jobID:
		return jobID, nil
	default:
		w.failJobQueued(jobID)
		return "", fmt.Errorf("worker busy: post-process trigger queue full")
	}
}

// CancelJob requests cooperative cancellation of a job: it flags the job in the
// store and cancels the in-flight pass's context when it is currently running
// on this worker (#113).
func (w *Worker) CancelJob(id string) error {
	if w.jobs == nil {
		return fmt.Errorf("jobs not available")
	}
	if err := w.jobs.RequestJobCancel(context.Background(), id); err != nil {
		return err
	}
	w.jobMu.Lock()
	if cancel, ok := w.jobCancels[id]; ok {
		cancel()
	}
	w.jobMu.Unlock()
	return nil
}

// Status returns a snapshot of pipeline metrics.
func (w *Worker) Status() any {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.snapshotLocked()
}

// MetricsSnapshot returns a typed copy of the current pipeline metrics, for
// callers (e.g. the Prometheus exporter) that need the concrete fields rather
// than the JSON-serialisable any from Status().
func (w *Worker) MetricsSnapshot() Metrics {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.snapshotLocked()
}

// snapshotLocked returns a copy of the metrics with ActiveStages populated from
// the current active set. Caller must hold w.mu.
func (w *Worker) snapshotLocked() Metrics {
	m := w.metrics // copy
	if len(w.active) > 0 {
		stages := make([]string, 0, len(w.active))
		for s := range w.active {
			stages = append(stages, s)
		}
		sort.Strings(stages)
		m.ActiveStages = stages
	}
	if w.scanActive {
		inflight := make([]string, 0, len(w.scanInFlight))
		for g := range w.scanInFlight {
			inflight = append(inflight, g)
		}
		sort.Strings(inflight)
		m.ScanProgress = &ScanProgress{
			InFlight:  inflight,
			Completed: w.scanCompleted,
			Total:     w.scanTotal,
			Backfill:  w.scanBackfill,
		}
	}
	return m
}

// --- metrics helpers ---

func (w *Worker) setStage(stage string) {
	w.mu.Lock()
	w.metrics.CurrentStage = stage
	w.mu.Unlock()
}

// stageStart marks a pipeline stage as currently running and records it as the
// current stage. stageEnd clears it. They are safe to call concurrently across
// loops, so ActiveStages reflects everything running at once.
func (w *Worker) stageStart(stage string) {
	w.mu.Lock()
	w.active[stage] = true
	w.metrics.CurrentStage = stage
	w.mu.Unlock()
}

func (w *Worker) stageEnd(stage string) {
	w.mu.Lock()
	delete(w.active, stage)
	// When nothing is running, reflect idle in the single-value CurrentStage
	// (compatibility with the pre-ActiveStages field/consumers).
	if len(w.active) == 0 {
		w.metrics.CurrentStage = "idle"
	}
	w.mu.Unlock()
}

func (w *Worker) recordError(err error) {
	w.recordStageError("", "", err)
}

// recordStageError records a pipeline failure with its stage and (optional)
// affected group into the bounded error history (#133), updates the last-error
// summary, logs it, and emits a notification. Capturing stage/group here means
// concurrent failures are all retained rather than overwriting one another.
func (w *Worker) recordStageError(stage, group string, err error) {
	if err == nil {
		return
	}
	msg := err.Error()
	w.log.Warn("pipeline stage error", "stage", stage, "group", group, "err", msg)

	w.mu.Lock()
	w.metrics.LastError = msg
	w.mu.Unlock()

	w.errMu.Lock()
	w.errSeq++
	e := PipelineError{Seq: w.errSeq, Stage: stage, Group: group, Message: msg, At: time.Now()}
	w.errHistory = append(w.errHistory, e)
	if len(w.errHistory) > maxErrHistory {
		w.errHistory = w.errHistory[len(w.errHistory)-maxErrHistory:]
	}
	w.errMu.Unlock()

	w.emit(notify.Event{
		Type:    notify.EventScanFailed,
		Title:   "Pipeline stage error",
		Message: msg,
		Fields:  fieldsFor(stage, group),
	})
}

// fieldsFor builds notification fields, omitting empty values.
func fieldsFor(stage, group string) map[string]string {
	f := map[string]string{}
	if stage != "" {
		f["stage"] = stage
	}
	if group != "" {
		f["group"] = group
	}
	if len(f) == 0 {
		return nil
	}
	return f
}

// RecentErrors returns up to limit most-recent pipeline errors, newest first
// (#133). limit <= 0 returns all retained errors. The history is in-memory and
// scoped to the current process lifetime.
func (w *Worker) RecentErrors(limit int) []PipelineError {
	w.errMu.Lock()
	defer w.errMu.Unlock()
	n := len(w.errHistory)
	if limit > 0 && limit < n {
		n = limit
	}
	out := make([]PipelineError, 0, n)
	for i := len(w.errHistory) - 1; i >= 0 && len(out) < n; i-- {
		out = append(out, w.errHistory[i])
	}
	return out
}

// recordGroupScan persists the per-group outcome of a scan/backfill pass (#114)
// when a recorder is attached. Best-effort: a failure to record is logged and
// swallowed so it never disrupts the pass. A cancelled pass (ctx done) is not
// recorded, since the outcome is incomplete and the write would fail anyway.
func (w *Worker) recordGroupScan(ctx context.Context, g store.Group, res scanner.ScanResult, scanErr error, dur time.Duration) {
	if w.scanRecorder == nil || ctx.Err() != nil {
		return
	}
	errMsg := ""
	if scanErr != nil {
		errMsg = scanErr.Error()
	}
	// Use a short-lived context detached from cancellation so a just-finished
	// pass still records even if the parent is about to be cancelled.
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := w.scanRecorder.RecordGroupScan(rctx, g.ID, store.GroupScanOutcome{
		Backfill:   w.currentPassBackfill(),
		Articles:   res.ArticlesPulled,
		Parts:      res.PartsInserted,
		ServerHigh: res.ServerHigh,
		DurationMS: dur.Milliseconds(),
		Err:        errMsg,
	}); err != nil {
		w.log.Warn("failed to record group scan state", "group", g.Name, "err", err)
	}
}

// currentPassBackfill reports whether the active scan pass is a backfill.
func (w *Worker) currentPassBackfill() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.scanBackfill
}

// scanPassBegin initialises pass-level progress for a scan/backfill of total
// groups.
func (w *Worker) scanPassBegin(total int, backfill bool) {
	w.mu.Lock()
	w.scanActive = true
	w.scanInFlight = map[string]bool{}
	w.scanCompleted = 0
	w.scanTotal = total
	w.scanBackfill = backfill
	w.mu.Unlock()
}

// scanPassEnd clears all scan progress when the pass finishes.
func (w *Worker) scanPassEnd() {
	w.mu.Lock()
	w.scanActive = false
	w.scanInFlight = nil
	w.scanCompleted = 0
	w.scanTotal = 0
	w.scanBackfill = false
	w.mu.Unlock()
}

// scanGroupBegin marks a group as currently being scanned.
func (w *Worker) scanGroupBegin(group string) {
	w.mu.Lock()
	if w.scanInFlight != nil {
		w.scanInFlight[group] = true
	}
	w.mu.Unlock()
}

// scanGroupEnd marks a group's scan finished and bumps the completed count.
func (w *Worker) scanGroupEnd(group string) {
	w.mu.Lock()
	delete(w.scanInFlight, group)
	w.scanCompleted++
	w.mu.Unlock()
}
