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
	// ScanInterval is how often the full pipeline cycle runs.
	ScanInterval time.Duration
	// PostProcessInterval is how often post-processing runs (may differ from
	// scanning to manage bandwidth). Zero means it runs each pipeline cycle.
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

// Worker runs and coordinates the pipeline.
type Worker struct {
	groups GroupLister
	scan   Scanner
	asm    Assembler
	build  ReleaseBuilder
	pp     PostProcessor
	log    *slog.Logger
	opts   Options

	// triggers carries manual job requests into the run loop.
	triggers chan trigger

	mu      sync.Mutex
	metrics Metrics
}

type triggerKind int

const (
	triggerCycle triggerKind = iota
	triggerScan
	triggerBackfill
)

type trigger struct {
	kind  triggerKind
	group string // empty = all active groups
}

// New creates a Worker.
func New(groups GroupLister, scan Scanner, asm Assembler, build ReleaseBuilder, pp PostProcessor, log *slog.Logger, opts Options) *Worker {
	if opts.ScanInterval <= 0 {
		opts.ScanInterval = 15 * time.Minute
	}
	if log == nil {
		log = slog.Default()
	}
	return &Worker{
		groups:   groups,
		scan:     scan,
		asm:      asm,
		build:    build,
		pp:       pp,
		log:      log,
		opts:     opts,
		triggers: make(chan trigger, 16),
	}
}

// Run drives the pipeline on a ticker until ctx is cancelled. It runs one cycle
// immediately, then on each interval, and whenever a manual trigger arrives.
// Run blocks until ctx is done, then returns.
func (w *Worker) Run(ctx context.Context) {
	w.log.Info("worker started", "scan_interval", w.opts.ScanInterval, "backfill", w.opts.EnableBackfill)
	ticker := time.NewTicker(w.opts.ScanInterval)
	defer ticker.Stop()

	// Kick off an initial cycle shortly after start.
	w.runCycle(ctx, "")

	for {
		select {
		case <-ctx.Done():
			w.log.Info("worker stopping")
			return
		case <-ticker.C:
			w.runCycle(ctx, "")
		case t := <-w.triggers:
			switch t.kind {
			case triggerScan:
				w.runScan(ctx, t.group, false)
				w.runDownstream(ctx)
			case triggerBackfill:
				w.runScan(ctx, t.group, true)
				w.runDownstream(ctx)
			default:
				w.runCycle(ctx, t.group)
			}
		}
	}
}

// runCycle runs the full pipeline once: scan (all or one group) -> assemble ->
// build -> post-process.
func (w *Worker) runCycle(ctx context.Context, group string) {
	start := time.Now()
	w.setRunning(true, start)
	defer w.setRunning(false, start)

	w.runScan(ctx, group, false)
	if w.opts.EnableBackfill {
		w.runScan(ctx, group, true)
	}
	w.runDownstream(ctx)

	w.mu.Lock()
	w.metrics.Cycles++
	end := time.Now()
	w.metrics.LastCycleEnd = &end
	w.metrics.CurrentStage = "idle"
	w.mu.Unlock()
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

// runDownstream runs assemble -> build -> post-process.
func (w *Worker) runDownstream(ctx context.Context) {
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

	if ctx.Err() != nil {
		return
	}
	w.setStage("postprocess")
	ppRes, err := w.pp.Run(ctx)
	if err != nil {
		w.recordError(fmt.Errorf("post-process: %w", err))
	} else {
		w.mu.Lock()
		w.metrics.ReleasesRenamed += int64(ppRes.Renamed)
		w.metrics.NFOsFound += int64(ppRes.NFOFound)
		w.mu.Unlock()
	}
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
	case w.triggers <- trigger{kind: triggerScan, group: group}:
		return nil
	default:
		return fmt.Errorf("worker busy: trigger queue full")
	}
}

// TriggerBackfill requests a backfill pass (non-blocking).
func (w *Worker) TriggerBackfill(group string) error {
	select {
	case w.triggers <- trigger{kind: triggerBackfill, group: group}:
		return nil
	default:
		return fmt.Errorf("worker busy: trigger queue full")
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

func (w *Worker) setRunning(running bool, start time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.metrics.Running = running
	if running {
		w.metrics.LastCycleStart = &start
	}
}

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
