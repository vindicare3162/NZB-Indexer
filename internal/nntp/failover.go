package nntp

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"
)

// Provider failover across NNTP servers (#128).
//
// The pipeline talks to a single logical NNTP source, but operators configure
// multiple database-managed servers with priorities. FailoverPool routes each
// operation to the highest-priority HEALTHY server, and on a connection-fatal
// error trips that server's circuit breaker and fails over to the next healthy
// one — so indexing continues through a fallback provider instead of failing.
// Opened circuits are probed after a cooldown (half-open) and restored on
// success. FailoverPool satisfies the same operation surface the pipeline uses
// (SelectGroupInfo, Overview, ListActive, Body), so it is a drop-in for *Pool.

// ErrNoHealthyServer is returned when every configured server's circuit is
// open (or none is configured), so no request can be sent.
var ErrNoHealthyServer = errors.New("nntp: no healthy news server available")

// EndpointConfig describes one provider endpoint for the failover pool.
type EndpointConfig struct {
	// ID/Name identify the endpoint (from the DB server row) for observability.
	ID   int64
	Name string
	// Priority orders selection (lower first), matching the DB ordering.
	Priority int
	// Config is the connection configuration for this provider.
	Config Config
}

// FailoverOptions tunes the circuit breaker.
type FailoverOptions struct {
	// FailureThreshold consecutive connection failures opens a circuit.
	FailureThreshold int
	// Cooldown is how long a circuit stays open before a probe is allowed.
	Cooldown time.Duration
}

type endpoint struct {
	cfg     EndpointConfig
	pool    *Pool
	breaker *circuitBreaker
}

// FailoverPool routes NNTP operations across prioritized providers with circuit
// breaking and failover.
type FailoverPool struct {
	mu        sync.RWMutex
	endpoints []*endpoint // sorted by priority (lower first)
	opts      FailoverOptions
	// newPool builds an underlying pool for an endpoint (injectable for tests).
	newPool func(Config) *Pool
}

// NewFailover builds a failover pool over the given endpoints. Endpoints are
// sorted by priority. newPool defaults to the real pool constructor.
func NewFailover(endpoints []EndpointConfig, opts FailoverOptions) *FailoverPool {
	fp := &FailoverPool{opts: opts, newPool: New}
	fp.SetEndpoints(endpoints)
	return fp
}

// newFailoverWithPool is a test hook: it uses a custom pool constructor.
func newFailoverWithPool(endpoints []EndpointConfig, opts FailoverOptions, newPool func(Config) *Pool) *FailoverPool {
	fp := &FailoverPool{opts: opts, newPool: newPool}
	fp.SetEndpoints(endpoints)
	return fp
}

// SetEndpoints replaces the configured endpoints (e.g. after an admin edit),
// closing pools for removed servers and building pools for new ones. Endpoints
// are matched by ID so unchanged servers keep their pool and circuit state.
func (fp *FailoverPool) SetEndpoints(cfgs []EndpointConfig) {
	sort.SliceStable(cfgs, func(i, j int) bool { return cfgs[i].Priority < cfgs[j].Priority })

	fp.mu.Lock()
	defer fp.mu.Unlock()

	existing := make(map[int64]*endpoint, len(fp.endpoints))
	for _, e := range fp.endpoints {
		existing[e.cfg.ID] = e
	}

	var next []*endpoint
	seen := make(map[int64]bool, len(cfgs))
	for _, c := range cfgs {
		seen[c.ID] = true
		if e, ok := existing[c.ID]; ok {
			// Reuse the pool + breaker; reconfigure connection params/capacity.
			e.cfg = c
			e.pool.Reconfigure(c.Config)
			next = append(next, e)
			continue
		}
		next = append(next, &endpoint{
			cfg:     c,
			pool:    fp.newPool(c.Config),
			breaker: newCircuitBreaker(fp.opts.FailureThreshold, fp.opts.Cooldown),
		})
	}
	// Close pools for endpoints no longer configured.
	for id, e := range existing {
		if !seen[id] {
			e.pool.Close()
		}
	}
	fp.endpoints = next
}

// Close closes all underlying pools.
func (fp *FailoverPool) Close() {
	fp.mu.Lock()
	defer fp.mu.Unlock()
	for _, e := range fp.endpoints {
		e.pool.Close()
	}
	fp.endpoints = nil
}

// run executes fn against the highest-priority healthy provider, failing over
// on connection-fatal errors. A protocol/retention error (the server answered)
// is returned to the caller immediately without failover — it is not a provider
// health problem. Auth errors trip the breaker and fail over. Returns
// ErrNoHealthyServer when no endpoint's circuit allows a request.
func (fp *FailoverPool) run(ctx context.Context, fn func(*Pool) error) error {
	fp.mu.RLock()
	eps := make([]*endpoint, len(fp.endpoints))
	copy(eps, fp.endpoints)
	fp.mu.RUnlock()

	if len(eps) == 0 {
		return ErrNoHealthyServer
	}

	var lastErr error
	attempted := false
	for _, e := range eps {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		allowed, _ := e.breaker.allow()
		if !allowed {
			continue // circuit open: skip this provider
		}
		attempted = true

		err := fn(e.pool)
		if err == nil {
			e.breaker.recordSuccess()
			return nil
		}

		kind := classifyError(err)
		switch kind {
		case ErrKindProtocol, ErrKindRetention:
			// The server answered; this is a per-request outcome, not a provider
			// health problem. Don't fail over or trip the breaker — but a
			// half-open probe reaching the server counts as a recovery.
			e.breaker.recordSuccess()
			return err
		default:
			// Connection or auth failure: trip the breaker and fail over.
			e.breaker.recordFailure(err)
			lastErr = err
		}
	}
	if !attempted {
		return ErrNoHealthyServer
	}
	return lastErr
}

// SelectGroupInfo returns the current article range for a group.
func (fp *FailoverPool) SelectGroupInfo(ctx context.Context, group string) (GroupInfo, error) {
	var info GroupInfo
	err := fp.run(ctx, func(p *Pool) error {
		gi, err := p.SelectGroupInfo(ctx, group)
		if err != nil {
			return err
		}
		info = gi
		return nil
	})
	return info, err
}

// Overview selects group and returns header summaries for [begin,end].
func (fp *FailoverPool) Overview(ctx context.Context, group string, begin, end int64) ([]Overview, error) {
	var out []Overview
	err := fp.run(ctx, func(p *Pool) error {
		ov, err := p.Overview(ctx, group, begin, end)
		if err != nil {
			return err
		}
		out = ov
		return nil
	})
	return out, err
}

// ListActive returns the groups the (selected) server carries.
func (fp *FailoverPool) ListActive(ctx context.Context) ([]AvailableGroup, error) {
	var groups []AvailableGroup
	err := fp.run(ctx, func(p *Pool) error {
		g, err := p.ListActive(ctx)
		if err != nil {
			return err
		}
		groups = g
		return nil
	})
	return groups, err
}

// Body fetches an article body, failing over across providers. A retention
// (430 "no such article") response from a provider is returned to the caller
// without failover — but callers that want to try another provider on
// retention misses can use BodyAnyProvider.
func (fp *FailoverPool) Body(ctx context.Context, messageID string) ([]byte, error) {
	var data []byte
	err := fp.run(ctx, func(p *Pool) error {
		b, err := p.Body(ctx, messageID)
		if err != nil {
			return err
		}
		data = b
		return nil
	})
	return data, err
}

// Stats reports aggregate pool utilisation across all endpoints (open + idle
// connections summed), so existing health reporting keeps working.
func (fp *FailoverPool) Stats() (open, idle int) {
	fp.mu.RLock()
	defer fp.mu.RUnlock()
	for _, e := range fp.endpoints {
		o, i := e.pool.Stats()
		open += o
		idle += i
	}
	return open, idle
}

// MaxConns reports the connection ceiling and in-use count of the currently
// active (highest-priority, non-open) provider, or the first endpoint when all
// are open.
func (fp *FailoverPool) MaxConns() (limit, inUse int) {
	fp.mu.RLock()
	defer fp.mu.RUnlock()
	if len(fp.endpoints) == 0 {
		return 0, 0
	}
	for _, e := range fp.endpoints {
		if e.breaker.snapshot().State != "open" {
			return e.pool.MaxConns()
		}
	}
	return fp.endpoints[0].pool.MaxConns()
}

// ServerHealth is the observable health of one provider.
type ServerHealth struct {
	ID                  int64  `json:"id"`
	Name                string `json:"name"`
	Priority            int    `json:"priority"`
	Circuit             string `json:"circuit"` // closed|open|half-open
	ConsecutiveFailures int    `json:"consecutive_failures"`
	LastError           string `json:"last_error,omitempty"`
	LastErrorKind       string `json:"last_error_kind,omitempty"`
	TotalFailures       int64  `json:"total_failures"`
	TotalSuccess        int64  `json:"total_success"`
	Opens               int64  `json:"circuit_opens"`
	PoolOpen            int    `json:"pool_open"`
	PoolIdle            int    `json:"pool_idle"`
	MaxConns            int    `json:"max_conns"`
}

// Health reports per-provider circuit/pool state for admin status and metrics.
func (fp *FailoverPool) Health() []ServerHealth {
	fp.mu.RLock()
	defer fp.mu.RUnlock()
	out := make([]ServerHealth, 0, len(fp.endpoints))
	for _, e := range fp.endpoints {
		snap := e.breaker.snapshot()
		open, idle := e.pool.Stats()
		limit, _ := e.pool.MaxConns()
		out = append(out, ServerHealth{
			ID: e.cfg.ID, Name: e.cfg.Name, Priority: e.cfg.Priority,
			Circuit: snap.State, ConsecutiveFailures: snap.ConsecutiveFailures,
			LastError: snap.LastError, LastErrorKind: snap.LastErrorKind,
			TotalFailures: snap.TotalFailures, TotalSuccess: snap.TotalSuccess,
			Opens: snap.Opens, PoolOpen: open, PoolIdle: idle, MaxConns: limit,
		})
	}
	return out
}

// ActiveServer reports the highest-priority server whose circuit is currently
// closed/half-open (the one that would serve the next request), or ("",0,false)
// when all circuits are open.
func (fp *FailoverPool) ActiveServer() (name string, id int64, ok bool) {
	fp.mu.RLock()
	defer fp.mu.RUnlock()
	for _, e := range fp.endpoints {
		snap := e.breaker.snapshot()
		if snap.State != "open" {
			return e.cfg.Name, e.cfg.ID, true
		}
	}
	return "", 0, false
}
