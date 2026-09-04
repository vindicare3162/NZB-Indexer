package server

import (
	"context"

	"github.com/vindicare/goindex/internal/nntp"
	"github.com/vindicare/goindex/internal/store"
)

// defaultJWTSecret is the placeholder used in examples/compose; flagged by the
// health probe so operators are reminded to set a real secret.
const defaultJWTSecret = "change-me-to-a-long-random-string"

// systemProbe implements rest.SystemProbe, exposing NNTP pool utilisation and a
// couple of config-sanity facts the REST layer can't see on its own.
type systemProbe struct {
	pool      *nntp.Pool
	store     *store.Store
	jwtSecret string
	// Effective concurrency sizing, captured at startup from the pool the
	// service actually built (active DB-managed server when present).
	nntpMaxConns  int
	scanWorkers   int
	ppWorkers     int
}

func (p systemProbe) NNTPPoolStats() (open, idle int) {
	if p.pool == nil {
		return 0, 0
	}
	return p.pool.Stats()
}

func (p systemProbe) NewsServerConfigured(ctx context.Context) bool {
	if p.store == nil {
		return false
	}
	_, err := p.store.GetActiveServer(ctx)
	return err == nil
}

func (p systemProbe) DefaultJWTSecret() bool {
	return p.jwtSecret == "" || p.jwtSecret == defaultJWTSecret
}

func (p systemProbe) Capacity() (nntpMaxConns, scanWorkers, postProcessWorkers int) {
	return p.nntpMaxConns, p.scanWorkers, p.ppWorkers
}
