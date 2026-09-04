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
	// AddReleaseIdentifier persists a normalized external identifier resolved by
	// a provider (#108/#134). Invalid/unsupported identifiers return an error and
	// store nothing; the enricher treats that as a non-fatal skip.
	AddReleaseIdentifier(ctx context.Context, releaseID int64, source, identifier string) error
}

// Options configures an enrichment pass.
type Options struct {
	// BatchLimit bounds how many releases one pass processes.
	BatchLimit int
	// RequestDelay throttles provider calls (a small pause between lookups) so
	// a keyless public API is not hammered. Zero means no delay.
	RequestDelay time.Duration
}

// Service enriches releases using one or more metadata providers (#134).
// Providers are tried in order; the first match supplies the release's metadata
// row, and identifiers from every matching provider are persisted (deduped by
// the store).
type Service struct {
	repo      Repo
	providers []metadata.Provider
	log       *slog.Logger
	opts      Options
}

// New creates an enrichment service with a single provider. When provider is
// nil the service is disabled and Run is a no-op. Kept for back-compat; prefer
// NewMulti for configurable multi-provider enrichment (#134).
func New(repo Repo, provider metadata.Provider, log *slog.Logger, opts Options) *Service {
	var providers []metadata.Provider
	if provider != nil {
		providers = []metadata.Provider{provider}
	}
	return NewMulti(repo, providers, log, opts)
}

// NewMulti creates an enrichment service that tries each provider in order
// (#134). An empty provider list disables the service.
func NewMulti(repo Repo, providers []metadata.Provider, log *slog.Logger, opts Options) *Service {
	if log == nil {
		log = slog.Default()
	}
	if opts.BatchLimit <= 0 {
		opts.BatchLimit = 100
	}
	// Drop nil providers defensively so Enabled/iteration are simple.
	kept := providers[:0]
	for _, p := range providers {
		if p != nil {
			kept = append(kept, p)
		}
	}
	return &Service{repo: repo, providers: kept, log: log, opts: opts}
}

// Enabled reports whether at least one provider is configured.
func (s *Service) Enabled() bool { return len(s.providers) > 0 }

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

		// Try each provider in order. The first match supplies the metadata row;
		// identifiers from every matching provider are collected and persisted.
		var (
			best      metadata.Result
			bestFound bool
			hadErr    bool
			idents    = map[string]string{} // source -> value, first writer wins
		)
		for _, p := range s.providers {
			if ctx.Err() != nil {
				return res, ctx.Err()
			}
			result, matched, lerr := p.Lookup(ctx, q)
			if lerr != nil {
				hadErr = true
				s.log.Warn("metadata lookup failed", "provider", p.Name(), "release", item.ID, "err", lerr)
				continue
			}
			if !matched {
				continue
			}
			if !bestFound {
				best = result
				bestFound = true
			}
			for src, val := range result.Identifiers {
				if _, ok := idents[src]; !ok {
					idents[src] = val
				}
			}
		}

		// A provider error with no match means nothing definitive was learned;
		// record nothing so the release is retried on a later pass.
		if !bestFound && hadErr {
			res.Errors++
			continue
		}

		in := store.ReleaseMetadataInput{ReleaseID: item.ID, Matched: bestFound}
		if bestFound {
			in.Title = best.Title
			in.Year = nonZero(best.Year)
			in.Season = nonZero(best.Season)
			in.Episode = nonZero(best.Episode)
			in.Source = best.Source
			in.ExternalID = best.ExternalID
			in.PosterURL = best.PosterURL
			in.Overview = best.Overview
			res.Matched++
		} else {
			res.Misses++
		}
		if err := s.repo.UpsertReleaseMetadata(ctx, in); err != nil {
			res.Errors++
			s.log.Warn("store metadata failed", "release", item.ID, "err", err)
		}
		// Persist any resolved external identifiers (#134). Invalid/unsupported
		// ones are skipped without failing the pass.
		for src, val := range idents {
			if err := s.repo.AddReleaseIdentifier(ctx, item.ID, src, val); err != nil {
				s.log.Debug("skip release identifier", "release", item.ID, "source", src, "err", err)
			}
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
