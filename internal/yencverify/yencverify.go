// Package yencverify determines the true segment count of articles whose
// Subject carried no "(n/m)" counter, by inspecting the yEnc header in the
// article body (#178).
//
// Without this, a single ~700KB fragment of a multi-gigabyte post looks
// identical to a genuinely standalone small file, and the assembler marks it
// complete. The yEnc header carries the real part/total/name, so a verified
// fragment can be routed into collection assembly (keyed on the yEnc name,
// which every segment of one file repeats) instead of being released as a
// stub.
package yencverify

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/vindicare/goindex/internal/nntp"
	"github.com/vindicare/goindex/internal/store"
	"github.com/vindicare/goindex/internal/yenc"
)

// Fetcher returns an article's leading yEnc control line by message-id. The
// NNTP pool satisfies this. Only the control line is fetched, not the whole
// body: at ~750KB per article and tens of millions of articles to classify,
// downloading bodies in full is the difference between a job that finishes and
// one that cannot.
type Fetcher interface {
	BodyHeader(ctx context.Context, messageID string) (string, error)
}

// Repo is the subset of the store this stage needs.
type Repo interface {
	ListPartsNeedingYencVerification(ctx context.Context, limit int) ([]store.YencCandidate, error)
	ApplyYencVerification(ctx context.Context, results []store.YencResult) error
	ListReleasesNeedingYencRepair(ctx context.Context, limit int) ([]store.YencRepairCandidate, error)
	ApplyYencRepair(ctx context.Context, results []store.YencRepairResult) error
}

// Options controls verification.
type Options struct {
	// BatchLimit bounds articles fetched per pass. Each one is a full article
	// body download, so this is the main bandwidth control.
	BatchLimit int
	// Concurrency is how many bodies are fetched in parallel; it should not
	// exceed the NNTP pool size.
	Concurrency int
	// FetchTimeout bounds a single body fetch.
	FetchTimeout time.Duration
}

// Verifier runs the verification and repair passes.
type Verifier struct {
	fetch Fetcher
	repo  Repo
	log   *slog.Logger
	opts  Options
}

// New creates a Verifier.
func New(fetch Fetcher, repo Repo, log *slog.Logger, opts Options) *Verifier {
	if opts.BatchLimit <= 0 {
		opts.BatchLimit = 100
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = 2
	}
	if opts.FetchTimeout <= 0 {
		opts.FetchTimeout = 60 * time.Second
	}
	if log == nil {
		log = slog.Default()
	}
	return &Verifier{fetch: fetch, repo: repo, log: log, opts: opts}
}

// Result summarises one pass.
type Result struct {
	Checked    int
	Standalone int
	Fragments  int
	Failed     int
}

// header is one article's resolved yEnc shape.
type header struct {
	ok    bool
	part  int
	total int
	name  string
}

// Run verifies a batch of unassigned ambiguous parts, so they can be assembled
// correctly rather than released as stubs.
func (v *Verifier) Run(ctx context.Context) (Result, error) {
	var res Result

	candidates, err := v.repo.ListPartsNeedingYencVerification(ctx, v.opts.BatchLimit)
	if err != nil {
		return res, fmt.Errorf("list yenc candidates: %w", err)
	}
	if len(candidates) == 0 {
		return res, nil
	}

	ids := make([]string, len(candidates))
	for i, c := range candidates {
		ids[i] = c.MessageID
	}
	headers := v.fetchHeaders(ctx, ids)

	results := make([]store.YencResult, 0, len(candidates))
	for i, c := range candidates {
		h := headers[i]
		results = append(results, store.YencResult{
			ID: c.ID, OK: h.ok, Total: h.total, Part: h.part, Name: h.name,
		})
		switch {
		case !h.ok:
			res.Failed++
		case h.total <= 1:
			res.Standalone++
		default:
			res.Fragments++
		}
		res.Checked++
	}
	if err := v.repo.ApplyYencVerification(ctx, results); err != nil {
		return res, fmt.Errorf("apply yenc verification: %w", err)
	}
	return res, nil
}

// RunRepair checks already-released stub releases against their real yEnc
// header and removes the ones that were never complete, resetting their part
// for proper reassembly. This is the backlog counterpart to Run: it exists
// because releases promoted before verification existed are still wrong.
func (v *Verifier) RunRepair(ctx context.Context) (Result, error) {
	var res Result

	candidates, err := v.repo.ListReleasesNeedingYencRepair(ctx, v.opts.BatchLimit)
	if err != nil {
		return res, fmt.Errorf("list yenc repair candidates: %w", err)
	}
	if len(candidates) == 0 {
		return res, nil
	}

	ids := make([]string, len(candidates))
	for i, c := range candidates {
		ids[i] = c.MessageID
	}
	headers := v.fetchHeaders(ctx, ids)

	results := make([]store.YencRepairResult, 0, len(candidates))
	for i, c := range candidates {
		h := headers[i]
		results = append(results, store.YencRepairResult{
			ReleaseID: c.ReleaseID, BinaryID: c.BinaryID, PartID: c.PartID,
			OK: h.ok, Total: h.total, Part: h.part, Name: h.name,
		})
		switch {
		case !h.ok:
			res.Failed++
		case h.total <= 1:
			res.Standalone++
		default:
			res.Fragments++
			if c.PartID == 0 {
				// The raw article row was already pruned by retention, so the
				// stub release can be removed but the segment cannot be
				// relinked without scanning the group again.
				v.log.Warn("yenc repair: fragment no longer has a raw part to relink",
					"release_id", c.ReleaseID, "message_id", c.MessageID, "total", h.total)
			}
		}
		res.Checked++
	}
	if err := v.repo.ApplyYencRepair(ctx, results); err != nil {
		return res, fmt.Errorf("apply yenc repair: %w", err)
	}
	return res, nil
}

// fetchHeaders resolves each message-id's yEnc header with a bounded worker
// pool, returning results positionally aligned with ids.
func (v *Verifier) fetchHeaders(ctx context.Context, ids []string) []header {
	out := make([]header, len(ids))

	workers := v.opts.Concurrency
	if workers > len(ids) {
		workers = len(ids)
	}
	if workers < 1 {
		workers = 1
	}

	var wg sync.WaitGroup
	jobs := make(chan int)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range jobs {
				if ctx.Err() != nil {
					return
				}
				out[idx] = v.fetchOne(ctx, ids[idx])
			}
		}()
	}
	for i := range ids {
		select {
		case jobs <- i:
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return out
		}
	}
	close(jobs)
	wg.Wait()
	return out
}

// fetchOne reads one article's yEnc control line and parses it.
func (v *Verifier) fetchOne(ctx context.Context, messageID string) header {
	fctx, cancel := context.WithTimeout(ctx, v.opts.FetchTimeout)
	defer cancel()

	line, err := v.fetch.BodyHeader(fctx, messageID)
	if err != nil {
		// An article with no yEnc payload at all (a plain text post — much of
		// the oldest backlog is ordinary discussion articles) is not a failed
		// lookup: it is a complete, single-article post, and it will never
		// become a yEnc post on a later attempt. Settle it as standalone
		// instead of burning the retry budget re-fetching it.
		if errors.Is(err, nntp.ErrNoYencHeader) {
			return header{ok: true, part: 1, total: 1}
		}
		v.log.Debug("yenc verify: fetch failed", "message_id", messageID, "err", err)
		return header{}
	}
	h, ok := yenc.ParseHeader([]byte(line))
	if !ok {
		return header{ok: true, part: 1, total: 1}
	}
	return header{ok: true, part: h.Part, total: h.Total, name: h.Name}
}
