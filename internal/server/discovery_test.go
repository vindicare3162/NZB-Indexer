package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vindicare/goindex/internal/nntp"
)

type fakeLister struct {
	groups []nntp.AvailableGroup
	calls  int
	err    error
}

func (f *fakeLister) ListActive(context.Context) ([]nntp.AvailableGroup, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.groups, nil
}

func sampleGroups() []nntp.AvailableGroup {
	return []nntp.AvailableGroup{
		{Name: "alt.binaries.movies", High: 5000, Low: 1, EstimatedCount: 5000, Status: "y"},
		{Name: "alt.binaries.tv", High: 3000, Low: 1, EstimatedCount: 3000, Status: "y"},
		{Name: "alt.binaries.music", High: 8000, Low: 1, EstimatedCount: 8000, Status: "y"},
		{Name: "comp.lang.go", High: 100, Low: 1, EstimatedCount: 100, Status: "y"},
	}
}

func TestDiscoverySearchFilterSortPage(t *testing.T) {
	fl := &fakeLister{groups: sampleGroups()}
	d := newDiscoveryService(fl, time.Hour)
	ctx := context.Background()

	// Filter to binaries, largest first.
	groups, total, _, err := d.SearchGroups(ctx, "binaries", 50, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if total != 3 {
		t.Errorf("total = %d, want 3", total)
	}
	if groups[0].Name != "alt.binaries.music" {
		t.Errorf("expected largest (music, 8000) first, got %q", groups[0].Name)
	}

	// Pagination: limit 1, offset 1 -> second-largest binary group (movies).
	page, total, _, err := d.SearchGroups(ctx, "binaries", 1, 1, false)
	if err != nil {
		t.Fatal(err)
	}
	if total != 3 || len(page) != 1 || page[0].Name != "alt.binaries.movies" {
		t.Errorf("paginated page = %+v (total %d)", page, total)
	}
}

func TestDiscoveryCachesAndRefreshes(t *testing.T) {
	fl := &fakeLister{groups: sampleGroups()}
	d := newDiscoveryService(fl, time.Hour)
	ctx := context.Background()

	// First call fetches.
	if _, _, _, err := d.SearchGroups(ctx, "", 50, 0, false); err != nil {
		t.Fatal(err)
	}
	// Second call within TTL uses cache (no extra fetch).
	if _, _, _, err := d.SearchGroups(ctx, "", 50, 0, false); err != nil {
		t.Fatal(err)
	}
	if fl.calls != 1 {
		t.Errorf("ListActive calls = %d, want 1 (cached)", fl.calls)
	}

	// Explicit refresh forces a re-fetch.
	if _, _, _, err := d.SearchGroups(ctx, "", 50, 0, true); err != nil {
		t.Fatal(err)
	}
	if fl.calls != 2 {
		t.Errorf("ListActive calls after refresh = %d, want 2", fl.calls)
	}
}

func TestDiscoveryPropagatesError(t *testing.T) {
	fl := &fakeLister{err: errors.New("provider down")}
	d := newDiscoveryService(fl, time.Hour)
	if _, _, _, err := d.SearchGroups(context.Background(), "", 50, 0, false); err == nil {
		t.Error("expected error to propagate when provider list fails")
	}
}
