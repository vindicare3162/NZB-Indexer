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

// buildEnricher constructs the metadata enrichment service from config (#134),
// building the ordered list of enabled providers. Returns nil when disabled or
// no known provider resolves, which keeps the worker's enrichment loop off.
func buildEnricher(st *store.Store, cfg config.Config, log *slog.Logger) worker.Enricher {
	if !cfg.Metadata.Enabled {
		return nil
	}

	// Resolve the configured provider list: prefer Providers, fall back to the
	// single legacy Provider, default to tvmaze.
	names := cfg.Metadata.Providers
	if len(names) == 0 {
		if p := strings.TrimSpace(cfg.Metadata.Provider); p != "" {
			names = []string{p}
		} else {
			names = []string{"tvmaze"}
		}
	}

	providers := buildProviders(names, cfg.Metadata.APIKeys, log)
	if len(providers) == 0 {
		log.Warn("no known metadata providers configured; enrichment disabled",
			"requested", names)
		return nil
	}
	svc := enrich.NewMulti(st, providers, log, enrich.Options{
		BatchLimit:   100,
		RequestDelay: 250 * time.Millisecond, // be gentle with keyless public APIs
	})
	active := make([]string, 0, len(providers))
	for _, p := range providers {
		active = append(active, p.Name())
	}
	log.Info("metadata enrichment enabled", "providers", active)
	return enricherAdapter{svc: svc}
}

// buildProviders instantiates the named metadata providers, skipping unknown
// names and keyed providers missing their API key (logged, not fatal).
func buildProviders(names []string, apiKeys map[string]string, log *slog.Logger) []metadata.Provider {
	var out []metadata.Provider
	seen := map[string]bool{}
	for _, raw := range names {
		name := strings.ToLower(strings.TrimSpace(raw))
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		switch name {
		case "tvmaze":
			// Keyless TV provider; also resolves imdb/tvdb/tmdb identifiers.
			out = append(out, metadata.NewTVMaze(nil))
		default:
			log.Warn("unknown metadata provider; skipping", "provider", name)
		}
	}
	return out
}
