package server

import (
	"context"
	"log/slog"

	"github.com/vindicare/goindex/internal/api/rest"
	"github.com/vindicare/goindex/internal/config"
	"github.com/vindicare/goindex/internal/search"
	"github.com/vindicare/goindex/internal/store"
)

// searchReindexer adapts search.Reindex into the rest.Reindexer interface
// (#139): it rebuilds the derived index from PostgreSQL, the authoritative
// source.
type searchReindexer struct {
	indexer search.Indexer
	store   *store.Store
}

func (s searchReindexer) Reindex(ctx context.Context) (int, error) {
	return search.Reindex(ctx, s.indexer, s.store.SearchReleasesPage, 0)
}

// buildSearch constructs the optional derived-search backend and reindexer from
// config (#139). When OpenSearch is disabled it returns (nil, nil): the REST
// API then searches PostgreSQL directly, and the reindex endpoint reports the
// feature is disabled. When enabled it returns a FallbackBackend that queries
// OpenSearch first and transparently falls back to PostgreSQL on error, plus a
// reindexer that rebuilds the OpenSearch index from PostgreSQL.
func buildSearch(cfg config.Config, st *store.Store, logger *slog.Logger) (rest.SearchBackend, rest.Reindexer) {
	if !cfg.OpenSearch.Enabled {
		return nil, nil
	}
	os := search.NewOpenSearchBackend(cfg.OpenSearch.URL, cfg.OpenSearch.Index, cfg.OpenSearch.Timeout)
	pg := search.NewPostgresBackend(st.SearchReleasesPage)
	backend := search.NewFallback(os, pg, func(err error) {
		logger.Warn("opensearch search failed; falling back to postgresql", "err", err)
	})
	return backend, searchReindexer{indexer: os, store: st}
}
