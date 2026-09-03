package server

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/vindicare/goindex/internal/api/rest"
	"github.com/vindicare/goindex/internal/nntp"
)

// groupLister is the subset of the NNTP pool the discovery service needs.
type groupLister interface {
	ListActive(ctx context.Context) ([]nntp.AvailableGroup, error)
}

// discoveryService caches the provider's group list (which is large and slow
// to fetch) and serves filtered, paginated views of it. It implements
// rest.Discoverer.
type discoveryService struct {
	pool groupLister
	ttl  time.Duration

	mu        sync.Mutex
	groups    []nntp.AvailableGroup
	fetchedAt time.Time
}

// newDiscoveryService creates a discovery service with the given cache TTL.
func newDiscoveryService(pool groupLister, ttl time.Duration) *discoveryService {
	if ttl <= 0 {
		ttl = time.Hour
	}
	return &discoveryService{pool: pool, ttl: ttl}
}

// SearchGroups returns a filtered, paginated slice of the provider's groups
// plus the total number of matches and the cache timestamp. The cache is
// refreshed when empty or older than the TTL, or when refresh is true. It
// implements rest.Discoverer.
func (d *discoveryService) SearchGroups(ctx context.Context, query string, limit, offset int, refresh bool) (groups []rest.DiscoveredGroup, total int, cachedAt time.Time, err error) {
	if err := d.ensureFresh(ctx, refresh); err != nil {
		return nil, 0, time.Time{}, err
	}

	d.mu.Lock()
	all := d.groups
	fetchedAt := d.fetchedAt
	d.mu.Unlock()

	q := strings.ToLower(strings.TrimSpace(query))
	var matched []nntp.AvailableGroup
	for _, g := range all {
		if q == "" || strings.Contains(strings.ToLower(g.Name), q) {
			matched = append(matched, g)
		}
	}
	total = len(matched)

	// Sort by estimated size (largest first) for relevance, then name.
	sort.Slice(matched, func(i, j int) bool {
		if matched[i].EstimatedCount != matched[j].EstimatedCount {
			return matched[i].EstimatedCount > matched[j].EstimatedCount
		}
		return matched[i].Name < matched[j].Name
	})

	if limit <= 0 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	end := offset + limit
	if offset > len(matched) {
		offset = len(matched)
	}
	if end > len(matched) {
		end = len(matched)
	}
	page := matched[offset:end]

	out := make([]rest.DiscoveredGroup, 0, len(page))
	for _, g := range page {
		out = append(out, rest.DiscoveredGroup{
			Name:           g.Name,
			EstimatedCount: g.EstimatedCount,
			Status:         g.Status,
		})
	}
	return out, total, fetchedAt, nil
}

// ensureFresh (re)loads the group list when the cache is empty, stale, or a
// refresh is explicitly requested.
func (d *discoveryService) ensureFresh(ctx context.Context, refresh bool) error {
	d.mu.Lock()
	stale := refresh || len(d.groups) == 0 || time.Since(d.fetchedAt) > d.ttl
	d.mu.Unlock()
	if !stale {
		return nil
	}

	groups, err := d.pool.ListActive(ctx)
	if err != nil {
		return err
	}
	d.mu.Lock()
	d.groups = groups
	d.fetchedAt = time.Now()
	d.mu.Unlock()
	return nil
}
