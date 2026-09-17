// Package reassemble repairs the grouping of parts that were parsed before the
// subject parser was fixed, and tears down the binaries they were wrongly
// grouped into so the normal assembler can rebuild them (#196).
package reassemble

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/vindicare/goindex/internal/scanner"
	"github.com/vindicare/goindex/internal/store"
)

// cursorKey names the persisted keyset cursor. Progress is stored rather than
// inferred so a pass resumes exactly where the last one stopped, across
// restarts, without rescanning 1.8 billion rows.
const cursorKey = "reassemble.parts_cursor"

// Repo is the subset of the store this needs.
type Repo interface {
	ListPartsForReparse(ctx context.Context, afterID int64, limit int) ([]store.PartForReparse, error)
	ApplyReparse(ctx context.Context, updates []store.PartReparse, dirty []int64) (store.ReassembleStats, error)
	GetSetting(ctx context.Context, key string) (string, bool, error)
	SetSetting(ctx context.Context, key, value string) error
}

// Options controls one pass.
type Options struct {
	// BatchSize is how many parts are read and re-parsed per batch.
	BatchSize int
	// MaxBatchesPerRun bounds a single pass so the task yields to the rest of
	// the pipeline instead of holding the database for an unbounded stretch.
	MaxBatchesPerRun int
	// MaxRunTime stops a pass early regardless of batch count, so a pass cannot
	// overrun its schedule on slow batches.
	MaxRunTime time.Duration
}

// Result summarises one pass.
type Result struct {
	BatchesRun        int
	PartsScanned      int
	PartsUpdated      int64
	PartsDetached     int64
	BinariesDeleted   int64
	ReleasesDeleted   int64
	BinariesProtected int64
	Cursor            int64
	Done              bool
}

// Reassembler re-parses stored subjects and rebuilds mis-grouped binaries.
type Reassembler struct {
	repo Repo
	log  *slog.Logger
	opts Options
}

// New creates a Reassembler.
func New(repo Repo, log *slog.Logger, opts Options) *Reassembler {
	if opts.BatchSize <= 0 {
		opts.BatchSize = 5000
	}
	if opts.MaxBatchesPerRun <= 0 {
		opts.MaxBatchesPerRun = 20
	}
	if opts.MaxRunTime <= 0 {
		opts.MaxRunTime = 2 * time.Minute
	}
	if log == nil {
		log = slog.Default()
	}
	return &Reassembler{repo: repo, log: log, opts: opts}
}

// Run advances the re-parse cursor by up to MaxBatchesPerRun batches. It is
// safe to stop between batches: the cursor is persisted after each one, and the
// batch itself is transactional.
func (r *Reassembler) Run(ctx context.Context) (Result, error) {
	var res Result

	cursor, err := r.cursor(ctx)
	if err != nil {
		return res, err
	}
	res.Cursor = cursor

	deadline := time.Now().Add(r.opts.MaxRunTime)
	for i := 0; i < r.opts.MaxBatchesPerRun; i++ {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		if time.Now().After(deadline) {
			break
		}

		parts, err := r.repo.ListPartsForReparse(ctx, cursor, r.opts.BatchSize)
		if err != nil {
			return res, err
		}
		if len(parts) == 0 {
			res.Done = true
			break
		}

		updates, dirty := Plan(parts)
		stats, err := r.applyWithRetry(ctx, updates, dirty)
		if err != nil {
			return res, err
		}

		cursor = parts[len(parts)-1].ID
		if err := r.repo.SetSetting(ctx, cursorKey, strconv.FormatInt(cursor, 10)); err != nil {
			return res, err
		}

		res.BatchesRun++
		res.PartsScanned += len(parts)
		res.PartsUpdated += stats.PartsUpdated
		res.PartsDetached += stats.PartsDetached
		res.BinariesDeleted += stats.BinariesDeleted
		res.ReleasesDeleted += stats.ReleasesDeleted
		res.BinariesProtected += stats.BinariesProtected
		res.Cursor = cursor
	}

	r.log.Info("reassembly pass complete",
		"batches", res.BatchesRun, "parts_scanned", res.PartsScanned,
		"parts_regrouped", res.PartsUpdated, "binaries_rebuilt", res.BinariesDeleted,
		"releases_removed", res.ReleasesDeleted, "binaries_protected", res.BinariesProtected,
		"cursor", res.Cursor, "done", res.Done)
	return res, nil
}

// Plan re-parses each part's stored subject and returns the grouping updates
// that differ, plus the distinct binaries those parts belong to.
//
// It is a pure function so the decision of what to rewrite — the risky half —
// is testable without a database.
func Plan(parts []store.PartForReparse) (updates []store.PartReparse, dirty []int64) {
	seen := make(map[int64]bool)
	for _, p := range parts {
		ps := scanner.ParseSubject(p.Subject)
		if ps.CollectionKey == p.CollectionKey &&
			ps.FileNumber == p.FileNumber &&
			ps.CollectionFiles == p.CollectionFiles &&
			ps.Normalized == p.NormSubject {
			continue // unchanged; re-parsing it again would be pure write amplification
		}
		updates = append(updates, store.PartReparse{
			ID:              p.ID,
			CollectionKey:   ps.CollectionKey,
			FileNumber:      ps.FileNumber,
			CollectionFiles: ps.CollectionFiles,
			NormSubject:     ps.Normalized,
		})
		if p.BinaryID != nil && !seen[*p.BinaryID] {
			seen[*p.BinaryID] = true
			dirty = append(dirty, *p.BinaryID)
		}
	}
	return updates, dirty
}

// maxDeadlockRetries bounds how often one batch is retried after a deadlock.
// Three is enough for contention that clears on its own without masking a
// genuine, persistent conflict.
const maxDeadlockRetries = 3

// applyWithRetry retries a batch that lost a deadlock.
//
// This task and the assembler both write `parts` — this one detaching parts and
// rewriting their grouping, the assembler claiming unassembled parts into
// binaries — so under load PostgreSQL picks one as the deadlock victim
// (SQLSTATE 40P01). Observed at roughly 2 failures per 30 passes in production.
//
// Losing is safe: the batch is one transaction, so it rolls back whole, and the
// cursor only advances after a successful apply, so no range is skipped. But an
// unretried batch wastes the scan that produced it and logs a task failure that
// looks like a fault. A deadlock is by definition transient, so retry it.
func (r *Reassembler) applyWithRetry(ctx context.Context, updates []store.PartReparse, dirty []int64) (store.ReassembleStats, error) {
	var last error
	for attempt := 0; attempt <= maxDeadlockRetries; attempt++ {
		stats, err := r.repo.ApplyReparse(ctx, updates, dirty)
		if err == nil {
			return stats, nil
		}
		if !store.IsDeadlock(err) {
			return stats, err
		}
		last = err
		if ctx.Err() != nil {
			return stats, ctx.Err()
		}
		// Brief, growing backoff so the retry does not collide with the same
		// assembler batch that just won.
		select {
		case <-ctx.Done():
			return stats, ctx.Err()
		case <-time.After(time.Duration(attempt+1) * 250 * time.Millisecond):
		}
		r.log.Debug("retrying reassembly batch after deadlock", "attempt", attempt+1)
	}
	return store.ReassembleStats{}, fmt.Errorf("batch still deadlocking after %d retries: %w", maxDeadlockRetries, last)
}

func (r *Reassembler) cursor(ctx context.Context) (int64, error) {
	raw, ok, err := r.repo.GetSetting(ctx, cursorKey)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, nil
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid persisted reassembly cursor %q: %w", raw, err)
	}
	return v, nil
}
