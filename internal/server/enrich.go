package server

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/vindicare/goindex/internal/config"
	"github.com/vindicare/goindex/internal/enrich"
	"github.com/vindicare/goindex/internal/metadata"
	"github.com/vindicare/goindex/internal/store"
	"github.com/vindicare/goindex/internal/worker"
)

// enricherAdapter adapts *enrich.Service to worker.Enricher: the worker wants a
// Run(ctx) error contract, while the service returns a result it does not need.
type enricherAdapter struct {
	svc *enrich.Service
}

func (e enricherAdapter) Enabled() bool { return e.svc != nil && e.svc.Enabled() }

func (e enricherAdapter) Run(ctx context.Context) error {
	if e.svc == nil {
		return nil
	}
	_, err := e.svc.Run(ctx)
	return err
}

// buildEnricher constructs the metadata enrichment service from config, or
// returns nil when disabled or no known provider is configured. Returning nil
// keeps the worker's enrichment loop entirely off.
func buildEnricher(st *store.Store, cfg config.Config, log *slog.Logger) worker.Enricher {
	if !cfg.Metadata.Enabled {
		return nil
	}
	var provider metadata.Provider
	switch strings.ToLower(strings.TrimSpace(cfg.Metadata.Provider)) {
	case "", "tvmaze":
		provider = metadata.NewTVMaze(nil)
	default:
		log.Warn("unknown metadata provider; enrichment disabled", "provider", cfg.Metadata.Provider)
		return nil
	}
	svc := enrich.New(st, provider, log, enrich.Options{
		BatchLimit:   100,
		RequestDelay: 250 * time.Millisecond, // be gentle with the keyless public API
	})
	log.Info("metadata enrichment enabled", "provider", provider.Name())
	return enricherAdapter{svc: svc}
}
