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
	ListCompleteUnreleasedBinaries(ctx context.Context, limit int) ([]store.Binary, error)
}

// Options controls assembly behaviour.
type Options struct {
	// BatchLimit bounds how many part-groupings are folded per Assemble call.
	BatchLimit int
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
	if log == nil {
		log = slog.Default()
	}
	return &Assembler{repo: repo, log: log, opts: opts}
}

// Result summarises one assembly pass.
type Result struct {
	BinariesTouched int
	StaleRemoved    int64
}

// Assemble folds pending parts into binaries and ages out stale incompletes.
func (a *Assembler) Assemble(ctx context.Context) (Result, error) {
	var res Result

	touched, err := a.repo.AssembleBinaries(ctx, a.opts.BatchLimit)
	if err != nil {
		return res, fmt.Errorf("assemble binaries: %w", err)
	}
	res.BinariesTouched = touched

	if a.opts.StaleAfter > 0 {
		removed, err := a.repo.AgeOutStaleBinaries(ctx, a.opts.StaleAfter)
		if err != nil {
			return res, fmt.Errorf("age out stale binaries: %w", err)
		}
		res.StaleRemoved = removed
	}

	a.log.Info("assembly pass complete",
		"binaries_touched", res.BinariesTouched, "stale_removed", res.StaleRemoved)
	return res, nil
}

// IsComplete reports whether a binary with the given collected/declared part
// counts is complete. A binary is complete only when its declared total is
// known (>0) and all parts have been collected.
func IsComplete(collected, declaredTotal int) bool {
	return declaredTotal > 0 && collected >= declaredTotal
}
