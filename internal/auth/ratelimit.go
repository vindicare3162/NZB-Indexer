package auth

import (
	"sync"
	"time"
)

// RateLimiter enforces a fixed-window request budget per key (typically an API
// key id). It is safe for concurrent use.
type RateLimiter struct {
	window time.Duration
	now    func() time.Time // injectable clock for tests

	mu      sync.Mutex
	windows map[string]*window
}

type window struct {
	count int
	reset time.Time
}

// NewRateLimiter creates a limiter with the given window duration.
func NewRateLimiter(windowDur time.Duration) *RateLimiter {
	if windowDur <= 0 {
		windowDur = time.Hour
	}
	return &RateLimiter{
		window:  windowDur,
		now:     time.Now,
		windows: make(map[string]*window),
	}
}

// Allow records a request for key against a per-window limit and reports
// whether it is permitted. A limit <= 0 means unlimited. It also returns the
// number of requests remaining in the current window and when it resets.
func (r *RateLimiter) Allow(key string, limit int) (allowed bool, remaining int, resetAt time.Time) {
	if limit <= 0 {
		return true, -1, time.Time{}
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	now := r.now()
	w, ok := r.windows[key]
	if !ok || now.After(w.reset) {
		w = &window{count: 0, reset: now.Add(r.window)}
		r.windows[key] = w
	}

	if w.count >= limit {
		return false, 0, w.reset
	}
	w.count++
	return true, limit - w.count, w.reset
}

// Cleanup removes expired windows to bound memory. Callers may invoke this
// periodically.
func (r *RateLimiter) Cleanup() {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	for k, w := range r.windows {
		if now.After(w.reset) {
			delete(r.windows, k)
		}
	}
}
