package nntp

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"
)

// Pool is a fixed-size pool of authenticated NNTP connections. Connections are
// created lazily up to Config.MaxConns and reused across operations. Callers
// normally use the high-level helpers (SelectGroupInfo, Overview, Body) rather
// than acquiring connections directly.
type Pool struct {
	cfg  Config
	dial dialer

	mu     sync.Mutex
	idle   []conn // ready-to-use connections
	open   int    // total live connections (idle + in use)
	closed bool

	// sem gates concurrent checkouts to the current MaxConns. Its capacity can
	// be resized at runtime (see Resize) without interrupting in-flight
	// checkouts or exceeding the provider budget during the transition.
	sem *resizableSem
}

// New creates a pool with the given configuration using real connections.
func New(cfg Config) *Pool {
	return newWithDialer(cfg, dialLib)
}

// Reconfigure updates the connection parameters (host, port, TLS, credentials,
// retry settings) used for new connections, and safely resizes the concurrency
// ceiling to cfg.MaxConns (#111). Existing idle connections are closed so
// subsequent dials use the new settings; in-use connections finish their
// current operation and are recycled on release.
//
// The MaxConns change takes effect immediately for future checkouts. Growing
// wakes waiting callers; shrinking stops granting new slots beyond the new
// limit while letting current holders finish — so the number of concurrent
// connections never exceeds either the old or the new limit during the
// transition, and no in-flight operation is interrupted.
func (p *Pool) Reconfigure(cfg Config) {
	newMax := cfg.MaxConns
	if newMax < 1 {
		newMax = 1
	}
	p.mu.Lock()
	cfg.MaxConns = newMax
	p.cfg = cfg
	idle := p.idle
	p.idle = nil
	p.open -= len(idle)
	p.mu.Unlock()

	// Resize the semaphore outside the pool lock (it has its own lock).
	p.sem.resize(newMax)

	// Close the now-stale idle connections. If we shrank, closing idle
	// connections also helps `open` converge toward the new limit; in-use
	// connections converge as they are released.
	for _, c := range idle {
		_ = c.close()
	}
}

// Resize changes only the connection ceiling, safely (see Reconfigure for the
// transition semantics). It is a convenience for callers that want to adjust
// capacity without touching connection parameters.
func (p *Pool) Resize(maxConns int) {
	if maxConns < 1 {
		maxConns = 1
	}
	p.mu.Lock()
	p.cfg.MaxConns = maxConns
	p.mu.Unlock()
	p.sem.resize(maxConns)
}

// MaxConns reports the current (effective) connection ceiling and how many
// connections are currently checked out.
func (p *Pool) MaxConns() (limit, inUse int) {
	return p.sem.stats()
}

// config returns a snapshot of the current pool configuration.
func (p *Pool) config() Config {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cfg
}

// newWithDialer creates a pool with a custom dialer (used by tests).
func newWithDialer(cfg Config, d dialer) *Pool {
	if cfg.MaxConns <= 0 {
		cfg.MaxConns = 1
	}
	return &Pool{
		cfg:  cfg,
		dial: d,
		sem:  newResizableSem(cfg.MaxConns),
	}
}

// acquire checks out a connection, creating one if the pool is below capacity.
// It blocks until a slot is free or ctx is done.
func (p *Pool) acquire(ctx context.Context) (conn, error) {
	if err := p.sem.acquire(ctx); err != nil {
		return nil, errors.Join(ErrNoConns, err)
	}

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		p.sem.release()
		return nil, ErrPoolClosed
	}
	// Reuse an idle connection if one is healthy.
	for len(p.idle) > 0 {
		c := p.idle[len(p.idle)-1]
		p.idle = p.idle[:len(p.idle)-1]
		p.mu.Unlock()
		if c.ping() == nil {
			return c, nil
		}
		// Unhealthy: discard and try the next idle connection.
		_ = c.close()
		p.mu.Lock()
		p.open--
	}
	p.mu.Unlock()

	// Need a new connection. Snapshot config under the lock to avoid racing
	// with Reconfigure.
	cfg := p.config()
	c, err := p.dial(cfg)
	if err != nil {
		p.sem.release()
		return nil, err
	}
	if err := c.authenticate(cfg.Username, cfg.Password); err != nil {
		_ = c.close()
		p.sem.release()
		return nil, err
	}
	p.mu.Lock()
	p.open++
	p.mu.Unlock()
	return c, nil
}

// release returns a connection to the pool, or discards it when broken is true
// or the pool is closed.
func (p *Pool) release(c conn, broken bool) {
	p.mu.Lock()
	if p.closed || broken {
		p.open--
		p.mu.Unlock()
		_ = c.close()
		p.sem.release()
		return
	}
	p.idle = append(p.idle, c)
	p.mu.Unlock()
	p.sem.release()
}

// Close discards all connections. The pool is unusable afterwards.
func (p *Pool) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	idle := p.idle
	p.idle = nil
	p.open -= len(idle)
	p.mu.Unlock()

	for _, c := range idle {
		_ = c.close()
	}
}

// Stats reports pool utilisation.
func (p *Pool) Stats() (open, idle int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.open, len(p.idle)
}

// withConn runs fn against a pooled connection, applying retry/backoff on
// transient failures. When fn returns an error deemed connection-fatal, the
// connection is discarded and (on remaining retries) a fresh one is dialed.
func (p *Pool) withConn(ctx context.Context, fn func(conn) error) error {
	attempts := p.config().MaxRetries + 1
	if attempts < 1 {
		attempts = 1
	}

	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			if err := sleepCtx(ctx, p.backoff(attempt)); err != nil {
				return err
			}
		}

		c, err := p.acquire(ctx)
		if err != nil {
			// Pool closed or context done are not retryable.
			if errors.Is(err, ErrPoolClosed) || ctx.Err() != nil {
				return err
			}
			lastErr = err
			continue
		}

		err = fn(c)
		if err == nil {
			p.release(c, false)
			return nil
		}

		// Connection-level failures invalidate the connection; retry with a
		// fresh one. Protocol-level errors (e.g. "no such group") are returned
		// to the caller immediately.
		if isConnFatal(err) {
			p.release(c, true)
			lastErr = err
			continue
		}
		p.release(c, false)
		return err
	}
	return lastErr
}

// backoff returns the delay before the given retry attempt (1-based).
func (p *Pool) backoff(attempt int) time.Duration {
	base := p.config().RetryBackoff
	if base <= 0 {
		base = 250 * time.Millisecond
	}
	return time.Duration(attempt) * base
}

// SelectGroupInfo returns the current article range for a group.
func (p *Pool) SelectGroupInfo(ctx context.Context, group string) (GroupInfo, error) {
	var info GroupInfo
	err := p.withConn(ctx, func(c conn) error {
		gi, err := c.selectGroup(group)
		if err != nil {
			return err
		}
		info = gi
		return nil
	})
	return info, err
}

// Overview selects group and returns header summaries for [begin,end].
func (p *Pool) Overview(ctx context.Context, group string, begin, end int64) ([]Overview, error) {
	var out []Overview
	err := p.withConn(ctx, func(c conn) error {
		if _, err := c.selectGroup(group); err != nil {
			return err
		}
		ov, err := c.overview(begin, end)
		if err != nil {
			return err
		}
		out = ov
		return nil
	})
	return out, err
}

// ListActive returns the groups the server carries. The list can be large
// (100k+ groups); callers should cache the result.
func (p *Pool) ListActive(ctx context.Context) ([]AvailableGroup, error) {
	var groups []AvailableGroup
	err := p.withConn(ctx, func(c conn) error {
		g, err := c.listActive()
		if err != nil {
			return err
		}
		groups = g
		return nil
	})
	return groups, err
}

// Body fetches and returns the full decoded body bytes of an article.
func (p *Pool) Body(ctx context.Context, messageID string) ([]byte, error) {
	var data []byte
	err := p.withConn(ctx, func(c conn) error {
		r, err := c.body(ctx, messageID)
		if err != nil {
			return err
		}
		defer r.Close()
		b, err := io.ReadAll(r)
		if err != nil {
			return err
		}
		data = b
		return nil
	})
	return data, err
}

// sleepCtx sleeps for d or until ctx is done.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
