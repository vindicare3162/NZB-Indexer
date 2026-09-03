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
	// DownstreamInterval is how often the downstream loop runs (assemble ->
	// build -> post-process), independently of scanning so a long scan cannot
	// starve post-processing. Zero defaults to ScanInterval.
	DownstreamInterval time.Duration
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

	// scanTriggers / downTriggers carry manual job requests into their loops.
	scanTriggers chan scanTrigger
	downTriggers chan downTrigger

	// scanMu serialises scan passes; downMu serialises downstream passes. They
	// are independent, so scan and downstream can run concurrently.
	scanMu sync.Mutex
	downMu sync.Mutex

	mu      sync.Mutex
	metrics Metrics
}

// scanTrigger is a manual scan request. backfill selects a backfill pass.
type scanTrigger struct {
	group    string // empty = all active groups
	backfill bool
}

// downKind selects which downstream stages a manual trigger runs.
type downKind int

const (
	downAll         downKind = iota // assemble -> build -> post-process
	downPostProcess                 // post-process only
)

type downTrigger struct{ kind downKind }

// New creates a Worker.
func New(groups GroupLister, scan Scanner, asm Assembler, build ReleaseBuilder, pp PostProcessor, log *slog.Logger, opts Options) *Worker {
	if opts.ScanInterval <= 0 {
		opts.ScanInterval = 15 * time.Minute
	}
	if opts.DownstreamInterval <= 0 {
		opts.DownstreamInterval = opts.ScanInterval
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
		downTriggers: make(chan downTrigger, 16),
	}
}

// Run drives the scan and downstream loops as independent goroutines until ctx
// is cancelled, then blocks until both have stopped. Because the loops are
// independent, a long-running scan does not delay assemble/build/post-process.
func (w *Worker) Run(ctx context.Context) {
	w.log.Info("worker started",
		"scan_interval", w.opts.ScanInterval,
		"downstream_interval", w.opts.DownstreamInterval,
		"backfill", w.opts.EnableBackfill)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); w.scanLoop(ctx) }()
	go func() { defer wg.Done(); w.downstreamLoop(ctx) }()
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

// downstreamLoop runs an initial downstream pass, then on DownstreamInterval
// and on manual triggers. It runs independently of scanning.
func (w *Worker) downstreamLoop(ctx context.Context) {
	ticker := time.NewTicker(w.opts.DownstreamInterval)
	defer ticker.Stop()

	w.runDownstream(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.runDownstream(ctx)
		case t := <-w.downTriggers:
			if t.kind == downPostProcess {
				w.runPostProcess(ctx)
			} else {
				w.runDownstream(ctx)
			}
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

// runDownstream runs assemble -> build -> post-process under the downstream
// mutex, so a scheduled pass and a manual trigger cannot overlap.
func (w *Worker) runDownstream(ctx context.Context) {
	w.downMu.Lock()
	defer w.downMu.Unlock()

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
		w.mu.Unlock()
	}()

	w.setStage("assemble")
	asmRes, err := w.asm.Assemble(ctx)
	if err != nil {
		w.recordError(fmt.Errorf("assemble: %w", err))
	} else {
		w.mu.Lock()
		w.metrics.BinariesTouched += int64(asmRes.BinariesTouched)
		w.mu.Unlock()
	}

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

	w.postProcess(ctx)

	w.mu.Lock()
	w.metrics.Cycles++
	end := time.Now()
	w.metrics.LastCycleEnd = &end
	w.metrics.CurrentStage = "idle"
	w.mu.Unlock()
}

// runPostProcess runs only the post-processing stage under the downstream
// mutex (used by the manual "post-process now" trigger).
func (w *Worker) runPostProcess(ctx context.Context) {
	w.downMu.Lock()
	defer w.downMu.Unlock()
	w.postProcess(ctx)
	w.setStage("idle")
}

// postProcess runs the post-processing stage and records metrics. The caller
// must hold downMu.
func (w *Worker) postProcess(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	w.setStage("postprocess")
	ppRes, err := w.pp.Run(ctx)
	if err != nil {
		w.recordError(fmt.Errorf("post-process: %w", err))
		return
	}
	w.mu.Lock()
	w.metrics.ReleasesRenamed += int64(ppRes.Renamed)
	w.metrics.NFOsFound += int64(ppRes.NFOFound)
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
// a scan or the downstream interval.
func (w *Worker) TriggerPostProcess() error {
	select {
	case w.downTriggers <- downTrigger{kind: downPostProcess}:
		return nil
	default:
		return fmt.Errorf("worker busy: downstream trigger queue full")
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
