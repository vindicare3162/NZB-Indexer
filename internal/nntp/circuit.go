package nntp

import (
	"sync"
	"time"
)

// Circuit breaking for NNTP providers (#128).
//
// A provider that is failing (network down, auth rejected, protocol garbage)
// should be isolated quickly so the pipeline fails over to a healthy fallback
// instead of hammering the dead provider (a "retry storm"). The circuitBreaker
// tracks consecutive failures per provider and, once a threshold is crossed,
// "opens" the circuit for a cooldown window during which the provider is
// skipped. After the cooldown a single probe is allowed (half-open); success
// closes the circuit and restores the provider, another failure re-opens it.

// circuitState is the breaker state.
type circuitState int

const (
	circuitClosed   circuitState = iota // healthy: requests allowed
	circuitOpen                         // failing: requests skipped until cooldown elapses
	circuitHalfOpen                     // cooldown elapsed: allow one probe
)

func (s circuitState) String() string {
	switch s {
	case circuitOpen:
		return "open"
	case circuitHalfOpen:
		return "half-open"
	default:
		return "closed"
	}
}

// circuitBreaker is a single provider's health/circuit state. Safe for
// concurrent use.
type circuitBreaker struct {
	mu sync.Mutex

	// threshold consecutive failures opens the circuit.
	threshold int
	// cooldown is how long the circuit stays open before a probe is allowed.
	cooldown time.Duration
	// now is injectable for deterministic tests (defaults to time.Now).
	now func() time.Time

	consecutiveFailures int
	state               circuitState
	openedAt            time.Time
	lastErr             string
	lastErrKind         ErrorKind
	// probing guards against concurrent half-open probes: only one caller may
	// probe at a time.
	probing bool

	// counters for observability.
	totalFailures int64
	totalSuccess  int64
	opens         int64
}

func newCircuitBreaker(threshold int, cooldown time.Duration) *circuitBreaker {
	if threshold < 1 {
		threshold = 3
	}
	if cooldown <= 0 {
		cooldown = 30 * time.Second
	}
	return &circuitBreaker{
		threshold: threshold,
		cooldown:  cooldown,
		now:       time.Now,
		state:     circuitClosed,
	}
}

// allow reports whether a request may be sent to this provider now. When the
// circuit is open but the cooldown has elapsed it transitions to half-open and
// allows exactly one probe (returns true, and probe=true); concurrent callers
// during a probe are denied so only one probe runs at a time.
func (c *circuitBreaker) allow() (ok bool, probe bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch c.state {
	case circuitClosed:
		return true, false
	case circuitHalfOpen:
		// A probe is already in flight (or pending); deny others.
		if c.probing {
			return false, false
		}
		c.probing = true
		return true, true
	default: // open
		if c.now().Sub(c.openedAt) >= c.cooldown {
			c.state = circuitHalfOpen
			c.probing = true
			return true, true
		}
		return false, false
	}
}

// recordSuccess closes the circuit and clears the failure count.
func (c *circuitBreaker) recordSuccess() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.consecutiveFailures = 0
	c.state = circuitClosed
	c.probing = false
	c.lastErr = ""
	c.lastErrKind = ErrKindNone
	c.totalSuccess++
}

// recordFailure advances the failure count and opens the circuit when the
// threshold is crossed. An authentication failure opens immediately (retrying
// won't help until credentials change).
func (c *circuitBreaker) recordFailure(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.probing = false
	c.consecutiveFailures++
	c.totalFailures++
	kind := classifyError(err)
	c.lastErrKind = kind
	if err != nil {
		c.lastErr = err.Error()
	}
	if kind == ErrKindAuth || c.consecutiveFailures >= c.threshold {
		if c.state != circuitOpen {
			c.opens++
		}
		c.state = circuitOpen
		c.openedAt = c.now()
	} else {
		// Not yet at threshold: stay closed but count the failure.
		if c.state == circuitHalfOpen {
			// A half-open probe failed: re-open.
			c.state = circuitOpen
			c.openedAt = c.now()
			c.opens++
		}
	}
}

// snapshot returns the current breaker state for observability.
type circuitSnapshot struct {
	State               string
	ConsecutiveFailures int
	LastError           string
	LastErrorKind       string
	TotalFailures       int64
	TotalSuccess        int64
	Opens               int64
}

func (c *circuitBreaker) snapshot() circuitSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	return circuitSnapshot{
		State:               c.state.String(),
		ConsecutiveFailures: c.consecutiveFailures,
		LastError:           c.lastErr,
		LastErrorKind:       c.lastErrKind.String(),
		TotalFailures:       c.totalFailures,
		TotalSuccess:        c.totalSuccess,
		Opens:               c.opens,
	}
}
