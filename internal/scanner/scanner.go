package scanner

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/vindicare/goindex/internal/nntp"
	"github.com/vindicare/goindex/internal/store"
)

// Source is the subset of the NNTP pool the scanner needs.
type Source interface {
	SelectGroupInfo(ctx context.Context, group string) (nntp.GroupInfo, error)
	Overview(ctx context.Context, group string, begin, end int64) ([]nntp.Overview, error)
}

// Repo is the subset of the store the scanner needs.
type Repo interface {
	GetGroupByName(ctx context.Context, name string) (store.Group, error)
	InsertParts(ctx context.Context, parts []store.PartInput) (int64, error)
	UpdateGroupForwardPosition(ctx context.Context, id, high int64) error
	UpdateGroupBackfillPosition(ctx context.Context, id, low int64, complete bool) error
}

// Options controls scanning behaviour.
type Options struct {
	// BatchSize is the number of articles fetched per XOVER call.
	BatchSize int64
	// ForwardMaxArticles caps how many articles a single forward-scan pass
	// ingests before yielding, so a firehose group cannot monopolise a cycle.
	// The watermark is persisted so the next pass resumes where this one
	// stopped. Zero means unbounded (scan up to the server high).
	ForwardMaxArticles int64
	// BackfillDays limits backfill to articles posted within this many days.
	// Zero disables the date bound.
	BackfillDays int
	// BackfillMaxArticles caps how many articles a single backfill pass walks
	// backwards. Zero means unlimited within one pass.
	BackfillMaxArticles int64
}

// Scanner ingests article headers into parts.
type Scanner struct {
	src  Source
	repo Repo
	log  *slog.Logger
	opts Options
}

// New creates a Scanner.
func New(src Source, repo Repo, log *slog.Logger, opts Options) *Scanner {
	if opts.BatchSize <= 0 {
		opts.BatchSize = 10000
	}
	if log == nil {
		log = slog.Default()
	}
	return &Scanner{src: src, repo: repo, log: log, opts: opts}
}

// ScanResult summarises one scan pass.
type ScanResult struct {
	Group          string
	ArticlesPulled int64
	PartsInserted  int64
	NewHigh        int64
	NewLow         int64
	BackfillDone   bool
	// ServerHigh is the server's current high-water article number observed
	// during this pass (0 when the server bounds could not be read). Combined
	// with the group's persisted forward watermark it gives the group's lag.
	ServerHigh int64
}

// ScanForward pulls new articles for a group from its stored high-water mark up
// to the server's current high. It advances the group's forward position as it
// goes, so an interrupted scan resumes without re-ingesting.
func (s *Scanner) ScanForward(ctx context.Context, groupName string) (ScanResult, error) {
	res := ScanResult{Group: groupName}

	g, err := s.repo.GetGroupByName(ctx, groupName)
	if err != nil {
		return res, fmt.Errorf("load group %q: %w", groupName, err)
	}
	info, err := s.src.SelectGroupInfo(ctx, groupName)
	if err != nil {
		return res, fmt.Errorf("select group %q: %w", groupName, err)
	}
	res.ServerHigh = info.High

	// Determine the starting point. On first scan (last=0) start from the
	// server low so we don't attempt the entire history as "forward".
	start := g.LastScannedHigh + 1
	if g.LastScannedHigh == 0 {
		start = info.Low
		// Seed the backfill watermark at our forward start so a later
		// backfill knows where the forward-scanned region begins.
		res.NewLow = start
	}
	if start < info.Low {
		// Our watermark fell out of retention; skip the expired gap.
		start = info.Low
	}
	res.NewHigh = g.LastScannedHigh

	if start > info.High {
		// Nothing new.
		res.NewHigh = g.LastScannedHigh
		return res, nil
	}

	// Bound how far this pass scans so a firehose group yields. The next pass
	// resumes from the persisted watermark.
	scanTo := info.High
	if s.opts.ForwardMaxArticles > 0 {
		if capped := start + s.opts.ForwardMaxArticles - 1; capped < scanTo {
			scanTo = capped
		}
	}

	for begin := start; begin <= scanTo; begin += s.opts.BatchSize {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		end := begin + s.opts.BatchSize - 1
		if end > scanTo {
			end = scanTo
		}

		pulled, inserted, err := s.ingestRange(ctx, g, begin, end)
		if err != nil {
			return res, err
		}
		res.ArticlesPulled += pulled
		res.PartsInserted += inserted

		// Advance and persist the watermark after each batch for resumability.
		if err := s.repo.UpdateGroupForwardPosition(ctx, g.ID, end); err != nil {
			return res, fmt.Errorf("persist forward position: %w", err)
		}
		res.NewHigh = end
	}

	s.log.Info("forward scan complete",
		"group", groupName, "pulled", res.ArticlesPulled,
		"inserted", res.PartsInserted, "high", res.NewHigh)
	return res, nil
}

// ScanBackfill walks backwards from the group's backfill watermark toward the
// server low (or the configured date/article bound), ingesting older articles.
func (s *Scanner) ScanBackfill(ctx context.Context, groupName string) (ScanResult, error) {
	res := ScanResult{Group: groupName}

	g, err := s.repo.GetGroupByName(ctx, groupName)
	if err != nil {
		return res, fmt.Errorf("load group %q: %w", groupName, err)
	}
	if g.BackfillComplete {
		res.BackfillDone = true
		res.NewLow = g.BackfillLow
		return res, nil
	}
	info, err := s.src.SelectGroupInfo(ctx, groupName)
	if err != nil {
		return res, fmt.Errorf("select group %q: %w", groupName, err)
	}
	res.ServerHigh = info.High

	// The upper bound of backfill is just below where we've already ingested.
	upper := g.BackfillLow - 1
	if g.BackfillLow == 0 {
		// Backfill hasn't started; begin just below the forward region.
		if g.LastScannedHigh > 0 {
			upper = g.LastScannedHigh // will be clamped by start computation below
		}
		upper = info.High
	}

	// Effective backfill limits: a per-group target overrides the global
	// default for each dimension when set.
	maxArticles := s.opts.BackfillMaxArticles
	if g.BackfillTargetArticles != nil {
		maxArticles = *g.BackfillTargetArticles
	}
	days := s.opts.BackfillDays
	if g.BackfillTargetDays != nil {
		days = *g.BackfillTargetDays
	}

	// The lowest article we're willing to fetch this pass.
	target := info.Low
	if maxArticles > 0 {
		if lim := upper - maxArticles + 1; lim > target {
			target = lim
		}
	}
	cutoff := time.Time{}
	if days > 0 {
		cutoff = time.Now().AddDate(0, 0, -days)
	}

	if upper < info.Low {
		// Already at or below the server low: nothing to backfill.
		if err := s.repo.UpdateGroupBackfillPosition(ctx, g.ID, info.Low, true); err != nil {
			return res, err
		}
		res.BackfillDone = true
		res.NewLow = info.Low
		return res, nil
	}

	res.NewLow = upper + 1
	reachedCutoff := false

	for end := upper; end >= target; end -= s.opts.BatchSize {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		begin := end - s.opts.BatchSize + 1
		if begin < target {
			begin = target
		}

		pulled, inserted, oldest, err := s.ingestRangeTracked(ctx, g, begin, end)
		if err != nil {
			return res, err
		}
		res.ArticlesPulled += pulled
		res.PartsInserted += inserted
		res.NewLow = begin

		// Persist progress each batch for resumability.
		if err := s.repo.UpdateGroupBackfillPosition(ctx, g.ID, begin, false); err != nil {
			return res, fmt.Errorf("persist backfill position: %w", err)
		}

		// Stop if we've passed the date cutoff.
		if !cutoff.IsZero() && !oldest.IsZero() && oldest.Before(cutoff) {
			reachedCutoff = true
			break
		}
		if begin == target {
			break
		}
	}

	// Backfill is complete when we've reached the server low or the date cutoff.
	done := res.NewLow <= info.Low || reachedCutoff
	if err := s.repo.UpdateGroupBackfillPosition(ctx, g.ID, res.NewLow, done); err != nil {
		return res, fmt.Errorf("persist backfill position: %w", err)
	}
	res.BackfillDone = done

	s.log.Info("backfill pass complete",
		"group", groupName, "pulled", res.ArticlesPulled,
		"inserted", res.PartsInserted, "low", res.NewLow, "done", done)
	return res, nil
}

// ingestRange fetches [begin,end], converts overviews to parts, and inserts
// them. Returns articles pulled and parts inserted.
func (s *Scanner) ingestRange(ctx context.Context, g store.Group, begin, end int64) (pulled, inserted int64, err error) {
	pulled, inserted, _, err = s.ingestRangeTracked(ctx, g, begin, end)
	return pulled, inserted, err
}

// ingestRangeTracked is like ingestRange but also reports the oldest posting
// time seen in the range (used by backfill to detect the date cutoff).
func (s *Scanner) ingestRangeTracked(ctx context.Context, g store.Group, begin, end int64) (pulled, inserted int64, oldest time.Time, err error) {
	ovs, err := s.src.Overview(ctx, g.Name, begin, end)
	if err != nil {
		return 0, 0, time.Time{}, fmt.Errorf("overview %d-%d: %w", begin, end, err)
	}
	if len(ovs) == 0 {
		return 0, 0, time.Time{}, nil
	}

	parts := make([]store.PartInput, 0, len(ovs))
	for _, ov := range ovs {
		if ov.MessageID == "" {
			continue // unusable without a message-id
		}
		ps := ParseSubject(ov.Subject)
		var postedAt *time.Time
		if !ov.Date.IsZero() {
			d := ov.Date
			postedAt = &d
			if oldest.IsZero() || d.Before(oldest) {
				oldest = d
			}
		}
		parts = append(parts, store.PartInput{
			GroupID:       g.ID,
			ArticleNumber: ov.ArticleNumber,
			MessageID:     ov.MessageID,
			Subject:       ov.Subject,
			Poster:        ov.From,
			PostedAt:      postedAt,
			Bytes:           ov.Bytes,
			PartNumber:      ps.PartNumber,
			TotalParts:      ps.TotalParts,
			NormSubject:     ps.Normalized,
			CollectionKey:   ps.CollectionKey,
			FileNumber:      ps.FileNumber,
			CollectionFiles: ps.CollectionFiles,
		})
	}

	ins, err := s.repo.InsertParts(ctx, parts)
	if err != nil {
		return int64(len(ovs)), 0, oldest, err
	}
	return int64(len(ovs)), ins, oldest, nil
}
