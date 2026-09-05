package store

import (
	"context"
	"testing"
)

// TestProjectCapacity validates the pure growth/retention projection math
// against independently-calculated expected values from known synthetic rates
// (#131 acceptance: tests validate calculations against known rates).
func TestProjectCapacity(t *testing.T) {
	// 10 articles/sec, 1000 bytes/article => 10*86400 = 864,000 arts/day,
	// 864,000,000 bytes/day.
	const artsPerSec = 10.0
	const bytesPerArt = 1000.0
	const dbBytes = 5_000_000_000 // 5 GB current
	const retentionDays = 30

	f := ProjectCapacity(dbBytes, artsPerSec, bytesPerArt, retentionDays, []int{30, 90, 365})

	if f.DailyArticles != 864000 {
		t.Errorf("daily articles = %v, want 864000", f.DailyArticles)
	}
	if f.DailyBytes != 864_000_000 {
		t.Errorf("daily bytes = %d, want 864000000", f.DailyBytes)
	}
	if f.RetentionDays != 30 {
		t.Errorf("retention days = %d, want 30", f.RetentionDays)
	}
	if len(f.Projections) != 3 {
		t.Fatalf("projections = %d, want 3", len(f.Projections))
	}

	// 30-day horizon: growth = 864,000,000 * 30 = 25,920,000,000.
	p30 := f.Projections[0]
	if p30.Days != 30 || p30.GrowthBytes != 25_920_000_000 {
		t.Errorf("30d growth = %d, want 25920000000", p30.GrowthBytes)
	}
	if p30.ProjectedDatabaseBytes != dbBytes+25_920_000_000 {
		t.Errorf("30d projected db = %d", p30.ProjectedDatabaseBytes)
	}
	// Retained (steady state) at 30-day window = daily * 30 = growth over 30d.
	if p30.RetainedBytes != 25_920_000_000 {
		t.Errorf("30d retained = %d, want 25920000000", p30.RetainedBytes)
	}

	// 365-day horizon growth = 864,000,000 * 365 = 315,360,000,000.
	p365 := f.Projections[2]
	if p365.Days != 365 || p365.GrowthBytes != 315_360_000_000 {
		t.Errorf("365d growth = %d, want 315360000000", p365.GrowthBytes)
	}
	// Retention is a fixed steady-state, independent of the horizon.
	if p365.RetainedBytes != 25_920_000_000 {
		t.Errorf("365d retained = %d, want 25920000000 (30-day steady state)", p365.RetainedBytes)
	}
}

// TestProjectCapacityDefaultsAndDisabledRetention covers the default horizons
// and the retention-disabled case.
func TestProjectCapacityDefaultsAndDisabledRetention(t *testing.T) {
	f := ProjectCapacity(0, 0, 0, 0, nil)
	if len(f.Projections) != 3 || f.Projections[0].Days != 30 || f.Projections[2].Days != 365 {
		t.Errorf("default horizons = %+v, want 30/90/365", f.Projections)
	}
	for _, p := range f.Projections {
		if p.RetainedBytes != 0 {
			t.Errorf("retention disabled should give 0 retained, got %d", p.RetainedBytes)
		}
		if p.GrowthBytes != 0 {
			t.Errorf("zero rate should give 0 growth, got %d", p.GrowthBytes)
		}
	}
	if f.Assumptions == "" {
		t.Error("forecast should document its assumptions")
	}
}

// TestCapacityStats covers the DB-backed size/rate gathering: table sizes are
// reported for known tables, retained part bytes and bytes-per-article are
// computed, and per-group storage rankings are ordered.
func TestCapacityStats(t *testing.T) {
	st := freshStore(t)
	ctx := context.Background()

	g1, _ := st.UpsertGroup(ctx, "alt.binaries.cap.one", true)
	g2, _ := st.UpsertGroup(ctx, "alt.binaries.cap.two", true)

	// g1: 3 parts * 1000 bytes; g2: 1 part * 1000 bytes.
	seedStatsParts(t, st, g1.ID, "capfile.one", "poster", 0, 3, 3)
	seedStatsParts(t, st, g2.ID, "capfile.two", "poster", 100, 1, 1)

	cs, err := st.CapacityStats(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if cs.DatabaseBytes <= 0 {
		t.Errorf("database bytes = %d, want > 0", cs.DatabaseBytes)
	}
	if len(cs.Tables) == 0 {
		t.Fatal("expected per-table sizes")
	}
	// The parts table must be present in the breakdown.
	var sawParts bool
	for _, tbl := range cs.Tables {
		if tbl.Name == "parts" {
			sawParts = true
		}
	}
	if !sawParts {
		t.Error("expected parts table in capacity breakdown")
	}
	if cs.PartsBytes != 4000 {
		t.Errorf("parts bytes = %d, want 4000", cs.PartsBytes)
	}
	if cs.BytesPerArticle != 1000 {
		t.Errorf("bytes/article = %v, want 1000", cs.BytesPerArticle)
	}
	// g1 (3000 bytes) should rank above g2 (1000 bytes).
	if len(cs.TopGroupsByStorage) != 2 {
		t.Fatalf("storage ranks = %d, want 2", len(cs.TopGroupsByStorage))
	}
	if cs.TopGroupsByStorage[0].Name != g1.Name || cs.TopGroupsByStorage[0].Bytes != 3000 {
		t.Errorf("top storage = %+v, want g1/3000", cs.TopGroupsByStorage[0])
	}
}
