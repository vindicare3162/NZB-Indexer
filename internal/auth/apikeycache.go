package auth

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/vindicare/goindex/internal/store"
)

// apiKeyCache is a bounded, short-TTL, in-process cache of resolved API-key
// records, plus per-key throttling of last-used writes. It cuts the two
// database operations that otherwise run on every authenticated Newznab
// request (a key/user lookup and a last_used_at update) down to at most one
// lookup per TTL window and one write per touch-interval per key.
//
// Revocation safety: entries expire after the TTL, so a deleted/deactivated key
// stops authenticating within that bound; callers may also Invalidate a key
// explicitly for immediate effect. No external store (Redis) is used — this is
// a single-instance optimisation.
type apiKeyCache struct {
	ttl           time.Duration
	touchInterval time.Duration
	maxEntries    int

	mu      sync.Mutex
	entries map[string]*apiKeyEntry

	// Metrics (atomic; read via Stats).
	hits, misses, evictions atomic.Int64
}

type apiKeyEntry struct {
	rec       store.APIKeyUser
	expiresAt time.Time
	// lastTouch is when we last persisted last_used_at for this key; used to
	// throttle writes.
	lastTouch time.Time
}

func newAPIKeyCache(ttl, touchInterval time.Duration, maxEntries int) *apiKeyCache {
	if ttl <= 0 {
		ttl = 60 * time.Second
	}
	if touchInterval <= 0 {
		touchInterval = 60 * time.Second
	}
	if maxEntries <= 0 {
		maxEntries = 4096
	}
	return &apiKeyCache{
		ttl:           ttl,
		touchInterval: touchInterval,
		maxEntries:    maxEntries,
		entries:       make(map[string]*apiKeyEntry),
	}
}

// get returns a cached, unexpired record for the key.
func (c *apiKeyCache) get(apiKey string) (store.APIKeyUser, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[apiKey]
	if !ok || time.Now().After(e.expiresAt) {
		if ok {
			delete(c.entries, apiKey)
		}
		c.misses.Add(1)
		return store.APIKeyUser{}, false
	}
	c.hits.Add(1)
	return e.rec, true
}

// put stores a resolved record, evicting an arbitrary entry if at capacity.
func (c *apiKeyCache) put(apiKey string, rec store.APIKeyUser) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.entries[apiKey]; !exists && len(c.entries) >= c.maxEntries {
		// Simple bounded eviction: drop one entry (map iteration order is
		// random, which is an acceptable eviction victim for a short-TTL cache).
		for k := range c.entries {
			delete(c.entries, k)
			c.evictions.Add(1)
			break
		}
	}
	c.entries[apiKey] = &apiKeyEntry{rec: rec, expiresAt: time.Now().Add(c.ttl)}
}

// shouldTouch reports whether last_used_at should be persisted for this key now
// (i.e. the throttle interval has elapsed), and records the touch time when it
// returns true. Keys not currently cached are always touched.
func (c *apiKeyCache) shouldTouch(apiKey string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[apiKey]
	if !ok {
		return true
	}
	if time.Since(e.lastTouch) < c.touchInterval {
		return false
	}
	e.lastTouch = time.Now()
	return true
}

// Invalidate removes a specific key from the cache (e.g. on deletion).
func (c *apiKeyCache) Invalidate(apiKey string) {
	c.mu.Lock()
	delete(c.entries, apiKey)
	c.mu.Unlock()
}

// InvalidateAll clears the cache (e.g. on a user deactivation when the specific
// keys aren't known to the caller).
func (c *apiKeyCache) InvalidateAll() {
	c.mu.Lock()
	c.entries = make(map[string]*apiKeyEntry)
	c.mu.Unlock()
}

// APIKeyCacheStats is a snapshot of cache activity for metrics.
type APIKeyCacheStats struct {
	Hits      int64
	Misses    int64
	Evictions int64
	Size      int
}

func (c *apiKeyCache) stats() APIKeyCacheStats {
	c.mu.Lock()
	size := len(c.entries)
	c.mu.Unlock()
	return APIKeyCacheStats{
		Hits:      c.hits.Load(),
		Misses:    c.misses.Load(),
		Evictions: c.evictions.Load(),
		Size:      size,
	}
}
