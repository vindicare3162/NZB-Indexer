// Package enrich runs metadata enrichment: it finds releases lacking metadata,
// parses their names, resolves them against a metadata.Provider, and persists
// the result (including definitive misses, so they are not retried endlessly).
// The service is a no-op when no provider is configured, so the feature stays
// optional and cannot break the pipeline.
package enrich

import (
	"context"
	"log/slog"
	"time"

	"github.com/vindicare/goindex/internal/metadata"
	"github.com/vindicare/goindex/internal/release"
	"github.com/vindicare/goindex/internal/store"
)

// Repo is the persistence the enricher needs.
type Repo interface {
	ListReleasesNeedingMetadata(ctx context.Context, limit int) ([]store.ReleaseForEnrichment, error)
	UpsertReleaseMetadata(ctx context.Context, in store.ReleaseMetadataInput) error
}

// Options configures an enrichment pass.
type Options struct {
	// BatchLimit bounds how many releases one pass processes.
	BatchLimit int
	// RequestDelay throttles provider calls (a small pause between lookups) so
	// a keyless public API is not hammered. Zero means no delay.
	RequestDelay time.Duration
}

// Service enriches releases using a metadata provider.
type Service struct {
	repo     Repo
	provider metadata.Provider
	log      *slog.Logger
	opts     Options
}

// New creates an enrichment service. When provider is nil the service is
// disabled and Run is a no-op.
func New(repo Repo, provider metadata.Provider, log *slog.Logger, opts Options) *Service {
	if log == nil {
		log = slog.Default()
	}
	if opts.BatchLimit <= 0 {
		opts.BatchLimit = 100
	}
	return &Service{repo: repo, provider: provider, log: log, opts: opts}
}

// Enabled reports whether a provider is configured.
func (s *Service) Enabled() bool { return s.provider != nil }

// Result summarises one enrichment pass.
type Result struct {
	Processed int
	Matched   int
	Misses    int
	Errors    int
}

// Run performs one enrichment pass over releases lacking metadata. It is safe
// to call when disabled (returns a zero result).
func (s *Service) Run(ctx context.Context) (Result, error) {
	var res Result
	if !s.Enabled() {
		return res, nil
	}

	items, err := s.repo.ListReleasesNeedingMetadata(ctx, s.opts.BatchLimit)
	if err != nil {
		return res, err
	}

	for _, item := range items {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		res.Processed++

		isTV := isTVCategory(item.CategoryID)
		q := metadata.ParseName(item.Name, isTV)

		result, matched, lerr := s.provider.Lookup(ctx, q)
		if lerr != nil {
			// Transient/provider error: record nothing so it retries next pass,
			// but count it and move on rather than aborting the batch.
			res.Errors++
			s.log.Warn("metadata lookup failed", "release", item.ID, "err", lerr)
			continue
		}

		in := store.ReleaseMetadataInput{ReleaseID: item.ID, Matched: matched}
		if matched {
			in.Title = result.Title
			in.Year = nonZero(result.Year)
			in.Season = nonZero(result.Season)
			in.Episode = nonZero(result.Episode)
			in.Source = result.Source
			in.ExternalID = result.ExternalID
			in.PosterURL = result.PosterURL
			in.Overview = result.Overview
			res.Matched++
		} else {
			res.Misses++
		}
		if err := s.repo.UpsertReleaseMetadata(ctx, in); err != nil {
			res.Errors++
			s.log.Warn("store metadata failed", "release", item.ID, "err", err)
		}

		if s.opts.RequestDelay > 0 {
			select {
			case <-ctx.Done():
				return res, ctx.Err()
			case <-time.After(s.opts.RequestDelay):
			}
		}
	}

	s.log.Info("metadata enrichment pass complete",
		"processed", res.Processed, "matched", res.Matched, "misses", res.Misses, "errors", res.Errors)
	return res, nil
}

// isTVCategory reports whether a category id is in the TV range (5000–5999).
func isTVCategory(cat *int) bool {
	return cat != nil && *cat >= release.CatTV && *cat < release.CatTV+1000
}

// nonZero returns a pointer to n, or nil when n is zero (so zero values are
// stored as SQL NULL rather than 0).
func nonZero(n int) *int {
	if n == 0 {
		return nil
	}
	return &n
}
