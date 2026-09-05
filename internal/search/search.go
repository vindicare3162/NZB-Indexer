// Package search provides an optional derived release-search backend (#139).
// PostgreSQL remains the authoritative system of record for releases and all
// pipeline state; this package can additionally mirror denormalized release
// documents into OpenSearch/Elasticsearch for richer fuzzy/ranked/faceted
// search. It is disabled by default and fully optional: when disabled — or when
// OpenSearch is unreachable — search cleanly falls back to the PostgreSQL
// backend, so single-instance deployments remain PostgreSQL-only with no
// behaviour change.
package search

import (
	"context"

	"github.com/vindicare/goindex/internal/store"
)

// Backend answers release searches. The PostgreSQL backend is authoritative;
// the OpenSearch backend is an optional derived index.
type Backend interface {
	// Search returns a page of releases for the filter.
	Search(ctx context.Context, f store.SearchFilter) (store.SearchResult, error)
	// Name identifies the backend for diagnostics/metrics ("postgres" or
	// "opensearch").
	Name() string
}

// Indexer mirrors release changes into a derived index (#139). Operations are
// idempotent so a change stream or full rebuild can be replayed safely. The
// PostgreSQL backend implements these as no-ops (it is the source of truth).
type Indexer interface {
	// IndexRelease upserts one release document (idempotent).
	IndexRelease(ctx context.Context, r store.Release) error
	// DeleteRelease removes a release document by guid (idempotent).
	DeleteRelease(ctx context.Context, guid string) error
}

// pgSearchFunc is the store's paginated search (store.SearchReleasesPage).
type pgSearchFunc func(ctx context.Context, f store.SearchFilter) (store.SearchResult, error)

// PostgresBackend is the default, authoritative search backend: it delegates to
// the store's PostgreSQL search (pg_trgm, keyset pagination). Indexing is a
// no-op because PostgreSQL is the source of truth.
type PostgresBackend struct {
	search pgSearchFunc
}

// NewPostgresBackend wraps the store's paginated search as a Backend.
func NewPostgresBackend(search pgSearchFunc) *PostgresBackend {
	return &PostgresBackend{search: search}
}

func (p *PostgresBackend) Name() string { return "postgres" }

func (p *PostgresBackend) Search(ctx context.Context, f store.SearchFilter) (store.SearchResult, error) {
	return p.search(ctx, f)
}

func (p *PostgresBackend) IndexRelease(context.Context, store.Release) error { return nil }
func (p *PostgresBackend) DeleteRelease(context.Context, string) error       { return nil }

// FallbackBackend wraps a primary (e.g. OpenSearch) backend and falls back to a
// secondary (PostgreSQL) when the primary errors, so a derived-index outage
// never breaks search (#139). Indexing forwards only to the primary (when it is
// also an Indexer); the secondary is authoritative and needs no indexing.
type FallbackBackend struct {
	primary   Backend
	secondary Backend
	// onFallback is called (best-effort) when a primary search fails and the
	// secondary is used, for logging/metrics. Optional.
	onFallback func(err error)
}

// NewFallback builds a fallback backend. primary and secondary must be non-nil.
func NewFallback(primary, secondary Backend, onFallback func(error)) *FallbackBackend {
	return &FallbackBackend{primary: primary, secondary: secondary, onFallback: onFallback}
}

func (f *FallbackBackend) Name() string { return f.primary.Name() + "+" + f.secondary.Name() }

func (f *FallbackBackend) Search(ctx context.Context, filter store.SearchFilter) (store.SearchResult, error) {
	res, err := f.primary.Search(ctx, filter)
	if err != nil {
		if f.onFallback != nil {
			f.onFallback(err)
		}
		return f.secondary.Search(ctx, filter)
	}
	return res, nil
}

// IndexRelease forwards to the primary when it is an Indexer.
func (f *FallbackBackend) IndexRelease(ctx context.Context, r store.Release) error {
	if ix, ok := f.primary.(Indexer); ok {
		return ix.IndexRelease(ctx, r)
	}
	return nil
}

// DeleteRelease forwards to the primary when it is an Indexer.
func (f *FallbackBackend) DeleteRelease(ctx context.Context, guid string) error {
	if ix, ok := f.primary.(Indexer); ok {
		return ix.DeleteRelease(ctx, guid)
	}
	return nil
}

// Reindex rebuilds the derived index from PostgreSQL (the authoritative source)
// (#139). It pages through every release in recency order and upserts each
// document via the indexer. Upserts are idempotent (keyed by guid), so a
// rebuild can be re-run safely after an OpenSearch outage or to recover from
// drift. It honours context cancellation between pages, so a rebuild can be
// stopped; because paging is by keyset cursor and upserts are idempotent, a
// cancelled rebuild can simply be started again to resume/complete.
//
// It returns the number of documents indexed. pageSize<=0 uses 500.
func Reindex(ctx context.Context, ix Indexer, search pgSearchFunc, pageSize int) (int, error) {
	if pageSize <= 0 {
		pageSize = 500
	}
	var (
		cursor  *store.SearchCursor
		indexed int
	)
	for {
		if err := ctx.Err(); err != nil {
			return indexed, err
		}
		page, err := search(ctx, store.SearchFilter{
			Limit:             pageSize,
			IncludeObfuscated: true,
			Cursor:            cursor,
			// Negative cap: we don't need a total for a rebuild, but the store
			// requires a filter; the count is ignored here.
			CountCap: -1,
		})
		if err != nil {
			return indexed, err
		}
		for _, r := range page.Releases {
			if err := ix.IndexRelease(ctx, r); err != nil {
				return indexed, err
			}
			indexed++
		}
		if !page.HasMore || page.NextCursor == nil || len(page.Releases) == 0 {
			return indexed, nil
		}
		cursor = page.NextCursor
	}
}
