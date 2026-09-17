// Package assembler groups scanned parts into binaries, determines which
// binaries are complete, and ages out stale incomplete ones.
package assembler

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/vindicare/goindex/internal/store"
)

// Repo is the subset of the store the assembler needs.
type Repo interface {
	AssembleBinaries(ctx context.Context, limit int) (int, error)
	AgeOutStaleBinaries(ctx context.Context, olderThan time.Duration) (int64, error)
	SettleQuietCollections(ctx context.Context, quietFor time.Duration, limit int) (int64, error)
	ListCompleteUnreleasedBinaries(ctx context.Context, limit int) ([]store.Binary, error)
}

// Options controls assembly behaviour.
type Options struct {
	// BatchLimit bounds how many part-groupings are folded per database batch.
	BatchLimit int
	// MaxBatchesPerRun caps how many batches a single Assemble call runs before
	// yielding, so a huge backlog is drained across a bounded amount of work
	// rather than one fixed batch. Zero means a sensible default; a negative
	// value means unlimited (drain fully).
	MaxBatchesPerRun int
	// SettleQuietAfter is how long a collection with no declared file count may
	// go without new parts before it is taken as complete. Must comfortably
	// exceed the scan interval, since a post's files arrive across one or two
	// passes. Zero disables settling.
	SettleQuietAfter time.Duration
	// StaleAfter is how long an incomplete binary may go without new parts
	// before it is aged out. Zero disables age-out.
	StaleAfter time.Duration
}

// Assembler folds parts into binaries.
type Assembler struct {
	repo Repo
	log  *slog.Logger
	opts Options
}

// New creates an Assembler.
func New(repo Repo, log *slog.Logger, opts Options) *Assembler {
	if opts.BatchLimit <= 0 {
		opts.BatchLimit = 500
	}
	if opts.MaxBatchesPerRun == 0 {
		// Default: enough batches to drain a large backlog in one cycle while
		// staying bounded (e.g. 500 * 200 = 100k groupings per run).
		opts.MaxBatchesPerRun = 200
	}
	if log == nil {
		log = slog.Default()
	}
	return &Assembler{repo: repo, log: log, opts: opts}
}

// Result summarises one assembly pass.
type Result struct {
	// BinariesTouched is the total groupings folded across all batches this run.
	BinariesTouched int
	// Batches is how many database batches ran.
	Batches int
	// Drained is true when the backlog was fully processed (a batch touched 0)
	// rather than stopping at the per-run batch cap.
	Drained      bool
	StaleRemoved int64
	// Settled is how many undeclared-count collections were completed on quiet
	// time this run.
	Settled int64
}

// Assemble folds pending parts into binaries and ages out stale incompletes.
// It repeatedly folds batches until the backlog is drained (a batch touches
// nothing) or the per-run batch cap is reached, so a large parts backlog is
// worked through in a single cycle instead of one fixed batch. It honours
// context cancellation between batches.
func (a *Assembler) Assemble(ctx context.Context) (Result, error) {
	var res Result

	maxBatches := a.opts.MaxBatchesPerRun
	res.Drained = true // assume drained unless we hit the cap
	for i := 0; maxBatches < 0 || i < maxBatches; i++ {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		touched, err := a.assembleBatchWithRetry(ctx)
		if err != nil {
			return res, fmt.Errorf("assemble binaries: %w", err)
		}
		if touched == 0 {
			break // backlog drained
		}
		res.BinariesTouched += touched
		res.Batches++

		// If this batch was full it likely means more remain; if it hit the
		// cap on the next iteration we'll mark not-drained.
		if maxBatches >= 0 && i == maxBatches-1 && touched > 0 {
			res.Drained = false
		}
	}

	// Collections whose file count was never declared cannot be measured against
	// a total, so they are completed once they stop receiving parts.
	if a.opts.SettleQuietAfter > 0 {
		settled, err := a.repo.SettleQuietCollections(ctx, a.opts.SettleQuietAfter, 0)
		if err != nil {
			return res, fmt.Errorf("settle quiet collections: %w", err)
		}
		res.Settled = settled
	}

	if a.opts.StaleAfter > 0 {
		removed, err := a.repo.AgeOutStaleBinaries(ctx, a.opts.StaleAfter)
		if err != nil {
			return res, fmt.Errorf("age out stale binaries: %w", err)
		}
		res.StaleRemoved = removed
	}

	a.log.Info("assembly pass complete",
		"binaries_touched", res.BinariesTouched, "batches", res.Batches,
		"drained", res.Drained, "settled", res.Settled, "stale_removed", res.StaleRemoved)
	return res, nil
}

// IsComplete reports whether a binary with the given collected/declared part
// counts is complete.
//
// When the declared total is known (>0), the binary is complete once all parts
// have been collected. When the declared total is unknown (0), the post carried
// no yEnc segment counter, which reliably indicates a single-article file; such
// a binary is complete as soon as its one article has been collected. Without
// this, single-article posts would never be released and would eventually be
// aged out and deleted.
func IsComplete(collected, declaredTotal int) bool {
	if declaredTotal > 0 {
		return collected >= declaredTotal
	}
	return collected >= 1
}

// maxDeadlockRetries bounds how often one batch is retried after losing a
// deadlock.
const maxDeadlockRetries = 3

// assembleBatchWithRetry runs one assembly batch, retrying if it is chosen as a
// deadlock victim.
//
// While re-assembly (#196) is running, both it and this loop write `parts`, and
// PostgreSQL picks one to abort. Measured against production that was 14
// failures across 27 assembly passes — more than half the assembler's work
// discarded, because a batch error aborts the whole pass. A deadlock is
// transient, so retry the batch instead of losing the pass.
func (a *Assembler) assembleBatchWithRetry(ctx context.Context) (int, error) {
	var last error
	for attempt := 0; attempt <= maxDeadlockRetries; attempt++ {
		touched, err := a.repo.AssembleBinaries(ctx, a.opts.BatchLimit)
		if err == nil {
			return touched, nil
		}
		if !store.IsDeadlock(err) {
			return 0, err
		}
		last = err
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(time.Duration(attempt+1) * 250 * time.Millisecond):
		}
	}
	return 0, last
}
