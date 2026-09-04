package server

import (
	"context"

	"github.com/vindicare/goindex/internal/api/rest"
	"github.com/vindicare/goindex/internal/nntp"
	"github.com/vindicare/goindex/internal/store"
)

// defaultJWTSecret is the placeholder used in examples/compose; flagged by the
// health probe so operators are reminded to set a real secret.
const defaultJWTSecret = "change-me-to-a-long-random-string"

// systemProbe implements rest.SystemProbe, exposing NNTP pool utilisation and a
// couple of config-sanity facts the REST layer can't see on its own.
type systemProbe struct {
	pool      *nntp.FailoverPool
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

// ServerHealth reports per-provider circuit/failover health (#128) for the
// admin health/status view.
func (p systemProbe) ServerHealth() []rest.ProviderHealth {
	if p.pool == nil {
		return nil
	}
	var out []rest.ProviderHealth
	for _, h := range p.pool.Health() {
		out = append(out, rest.ProviderHealth{
			Name: h.Name, Priority: h.Priority, Circuit: h.Circuit,
			ConsecutiveFailures: h.ConsecutiveFailures, LastError: h.LastError,
			LastErrorKind: h.LastErrorKind, TotalFailures: h.TotalFailures,
			TotalSuccess: h.TotalSuccess, CircuitOpens: h.Opens,
			PoolOpen: h.PoolOpen, PoolIdle: h.PoolIdle, MaxConns: h.MaxConns,
		})
	}
	return out
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
	// Report the LIVE pool ceiling so an admin change to a server's
	// max-connections (which safely resizes the pool, #111) is reflected in
	// health/status rather than the startup value. Worker counts are the
	// startup budget; their real parallelism is capped by this live ceiling.
	max := p.nntpMaxConns
	if p.pool != nil {
		if lim, _ := p.pool.MaxConns(); lim > 0 {
			max = lim
		}
	}
	return max, p.scanWorkers, p.ppWorkers
}
