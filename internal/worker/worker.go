// Package worker orchestrates the goindex pipeline stages (scan -> assemble ->
// release -> post-process) as scheduled background jobs, with manual triggers,
// graceful shutdown, and status/metrics reporting.
package worker

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/vindicare/goindex/internal/assembler"
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
	CurrentStage     string     `json:"current_stage"`
}

// Worker runs and coordinates the pipeline. The scan loop and the downstream
// (assemble -> build -> post-process) loop run as independent goroutines so a
// long-running scan cannot starve post-processing.
type Worker struct {
	groups GroupLister
	scan   Scanner
	asm    Assembler
	build  ReleaseBuilder
	pp     PostProcessor
	log    *slog.Logger
	opts   Options

	// Manual job requests are delivered into their respective loops.
	scanTriggers chan scanTrigger
	ppTriggers   chan struct{}

	// Each loop has its own mutex so scan, assemble/build, and post-process run
	// concurrently while each is serialised internally.
	scanMu  sync.Mutex
	asmMu   sync.Mutex
	buildMu sync.Mutex
	ppMu    sync.Mutex

	mu      sync.Mutex
	metrics Metrics
}

// scanTrigger is a manual scan request. backfill selects a backfill pass.
type scanTrigger struct {
	group    string // empty = all active groups
	backfill bool
}

// New creates a Worker.
func New(groups GroupLister, scan Scanner, asm Assembler, build ReleaseBuilder, pp PostProcessor, log *slog.Logger, opts Options) *Worker {
	if opts.ScanInterval <= 0 {
		opts.ScanInterval = 15 * time.Minute
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
		log:          log,
		opts:         opts,
		scanTriggers: make(chan scanTrigger, 16),
		ppTriggers:   make(chan struct{}, 16),
	}
}

// Run drives the scan, assemble/build, and post-process loops as independent
// goroutines until ctx is cancelled, then blocks until all have stopped.
// Because the loops are independent, neither a long-running scan nor a large
// assemble backlog can starve post-processing (name recovery).
func (w *Worker) Run(ctx context.Context) {
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
	wg.Wait()
	w.log.Info("worker stopped")
}

// scanLoop runs an initial scan, then on ScanInterval and on manual triggers.
func (w *Worker) scanLoop(ctx context.Context) {
	ticker := time.NewTicker(w.opts.ScanInterval)
	defer ticker.Stop()

	w.doScan(ctx, "", false)
	if w.opts.EnableBackfill {
		w.doScan(ctx, "", true)
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.doScan(ctx, "", false)
			if w.opts.EnableBackfill {
				w.doScan(ctx, "", true)
			}
		case t := <-w.scanTriggers:
			w.doScan(ctx, t.group, t.backfill)
		}
	}
}

// assembleLoop runs an initial assemble pass, then on DownstreamInterval. It
// folds parts into binaries independently of scanning, building, and
// post-processing.
func (w *Worker) assembleLoop(ctx context.Context) {
	ticker := time.NewTicker(w.opts.DownstreamInterval)
	defer ticker.Stop()

	w.runAssemble(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.runAssemble(ctx)
		}
	}
}

// buildLoop promotes complete binaries into releases on BuildInterval,
// independently of assembly. This matters at scale: a large parts backlog can
// keep the assemble loop busy for a long time, but complete binaries must still
// become releases promptly instead of waiting for assembly to fully drain.
func (w *Worker) buildLoop(ctx context.Context) {
	ticker := time.NewTicker(w.opts.BuildInterval)
	defer ticker.Stop()

	w.runBuild(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.runBuild(ctx)
		}
	}
}

// postProcessLoop runs an initial post-process pass, then on
// PostProcessInterval and on manual triggers. It runs independently of both
// scanning and assemble/build, so a large parts backlog cannot starve name
// recovery.
func (w *Worker) postProcessLoop(ctx context.Context) {
	ticker := time.NewTicker(w.opts.PostProcessInterval)
	defer ticker.Stop()

	w.runPostProcess(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.runPostProcess(ctx)
		case <-w.ppTriggers:
			w.runPostProcess(ctx)
		}
	}
}

// doScan runs one scan pass under the scan mutex (so scans don't overlap).
func (w *Worker) doScan(ctx context.Context, group string, backfill bool) {
	w.scanMu.Lock()
	defer w.scanMu.Unlock()
	w.runScan(ctx, group, backfill)
}

// runScan scans a single named group or all active groups.
func (w *Worker) runScan(ctx context.Context, group string, backfill bool) {
	stage := "scan"
	if backfill {
		stage = "backfill"
	}
	w.setStage(stage)

	groups, err := w.targetGroups(ctx, group)
	if err != nil {
		w.recordError(fmt.Errorf("list groups: %w", err))
		return
	}

	for _, g := range groups {
		if ctx.Err() != nil {
			return
		}
		var (
			res scanner.ScanResult
			err error
		)
		if backfill {
			res, err = w.scan.ScanBackfill(ctx, g.Name)
		} else {
			res, err = w.scan.ScanForward(ctx, g.Name)
		}
		if err != nil {
			w.recordError(fmt.Errorf("scan %s: %w", g.Name, err))
			continue
		}
		w.mu.Lock()
		w.metrics.ArticlesPulled += res.ArticlesPulled
		w.metrics.PartsInserted += res.PartsInserted
		w.mu.Unlock()
	}
}

// runAssemble folds parts into binaries under the assemble mutex, so a
// scheduled pass and a manual trigger cannot overlap. It runs independently of
// building and post-processing.
func (w *Worker) runAssemble(ctx context.Context) {
	w.asmMu.Lock()
	defer w.asmMu.Unlock()

	if ctx.Err() != nil {
		return
	}
	w.setStage("assemble")
	asmRes, err := w.asm.Assemble(ctx)
	if err != nil {
		w.recordError(fmt.Errorf("assemble: %w", err))
	} else {
		w.mu.Lock()
		w.metrics.BinariesTouched += int64(asmRes.BinariesTouched)
		w.mu.Unlock()
	}
	w.setStage("idle")
}

// runBuild promotes complete binaries into releases under the build mutex. It
// runs independently of assembly, so complete binaries become releases
// promptly even while a large parts backlog keeps the assemble loop busy.
func (w *Worker) runBuild(ctx context.Context) {
	w.buildMu.Lock()
	defer w.buildMu.Unlock()

	if ctx.Err() != nil {
		return
	}
	w.setStage("release")
	buildRes, err := w.build.Build(ctx)
	if err != nil {
		w.recordError(fmt.Errorf("build releases: %w", err))
	} else {
		w.mu.Lock()
		w.metrics.ReleasesCreated += int64(buildRes.Created)
		w.mu.Unlock()
	}
	w.setStage("idle")
}

// runPostProcess runs one post-processing pass under the post-process mutex,
// independently of scanning and assemble/build. It records metrics and counts
// a completed pass as a cycle.
func (w *Worker) runPostProcess(ctx context.Context) {
	w.ppMu.Lock()
	defer w.ppMu.Unlock()

	if ctx.Err() != nil {
		return
	}
	start := time.Now()
	w.mu.Lock()
	w.metrics.Running = true
	w.metrics.LastCycleStart = &start
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		w.metrics.Running = false
		w.metrics.CurrentStage = "idle"
		w.mu.Unlock()
	}()

	w.setStage("postprocess")
	ppRes, err := w.pp.Run(ctx)
	if err != nil {
		w.recordError(fmt.Errorf("post-process: %w", err))
		return
	}
	w.mu.Lock()
	w.metrics.ReleasesRenamed += int64(ppRes.Renamed)
	w.metrics.NFOsFound += int64(ppRes.NFOFound)
	w.metrics.Cycles++
	end := time.Now()
	w.metrics.LastCycleEnd = &end
	w.mu.Unlock()
}

// targetGroups resolves the group set for a scan: one named group or all active.
func (w *Worker) targetGroups(ctx context.Context, group string) ([]store.Group, error) {
	if group != "" {
		return []store.Group{{Name: group}}, nil
	}
	return w.groups.ListGroups(ctx, true)
}

// --- rest.JobController implementation ---

// TriggerScan requests a forward scan (non-blocking).
func (w *Worker) TriggerScan(group string) error {
	select {
	case w.scanTriggers <- scanTrigger{group: group, backfill: false}:
		return nil
	default:
		return fmt.Errorf("worker busy: scan trigger queue full")
	}
}

// TriggerBackfill requests a backfill pass (non-blocking).
func (w *Worker) TriggerBackfill(group string) error {
	select {
	case w.scanTriggers <- scanTrigger{group: group, backfill: true}:
		return nil
	default:
		return fmt.Errorf("worker busy: scan trigger queue full")
	}
}

// TriggerPostProcess requests an immediate post-processing pass (non-blocking).
// This lets an operator recover names for pending releases without waiting for
// a scan or the post-process interval. It contends only with the post-process
// loop, so it runs even while a large assemble backlog is being worked.
func (w *Worker) TriggerPostProcess() error {
	select {
	case w.ppTriggers <- struct{}{}:
		return nil
	default:
		return fmt.Errorf("worker busy: post-process trigger queue full")
	}
}

// Status returns a snapshot of pipeline metrics.
func (w *Worker) Status() any {
	w.mu.Lock()
	defer w.mu.Unlock()
	m := w.metrics // copy
	return m
}

// --- metrics helpers ---

func (w *Worker) setStage(stage string) {
	w.mu.Lock()
	w.metrics.CurrentStage = stage
	w.mu.Unlock()
}

func (w *Worker) recordError(err error) {
	w.log.Warn("pipeline stage error", "err", err)
	w.mu.Lock()
	w.metrics.LastError = err.Error()
	w.mu.Unlock()
}
