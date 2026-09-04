package store

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

// testDSN returns the integration-test database DSN, or skips the test when
// GOINDEX_TEST_DSN is unset so the default `go test ./...` run needs no DB.
func testDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("GOINDEX_TEST_DSN")
	if dsn == "" {
		t.Skip("GOINDEX_TEST_DSN not set; skipping PostgreSQL integration test")
	}
	return dsn
}

// freshStore rolls the schema all the way down, applies it up, and returns an
// open Store. This guarantees each test starts from a known state.
func freshStore(t *testing.T) *Store {
	t.Helper()
	dsn := testDSN(t)

	if err := MigrateDown(dsn); err != nil {
		t.Fatalf("migrate down: %v", err)
	}
	if err := Migrate(dsn); err != nil {
		t.Fatalf("migrate up: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st, err := Open(ctx, dsn, 5)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)
	return st
}

func TestMigrateUpDownAndVersion(t *testing.T) {
	dsn := testDSN(t)

	if err := MigrateDown(dsn); err != nil {
		t.Fatalf("initial migrate down: %v", err)
	}
	if err := Migrate(dsn); err != nil {
		t.Fatalf("migrate up: %v", err)
	}

	v, dirty, err := MigrationVersion(dsn)
	if err != nil {
		t.Fatalf("version: %v", err)
	}
	if dirty {
		t.Fatal("schema is dirty after migrate up")
	}
	if v != 13 {
		t.Fatalf("expected schema version 13, got %d", v)
	}

	// Re-running up should be a no-op, not an error.
	if err := Migrate(dsn); err != nil {
		t.Fatalf("re-run migrate up: %v", err)
	}

	// Down should clear the schema without error.
	if err := MigrateDown(dsn); err != nil {
		t.Fatalf("migrate down: %v", err)
	}
	// Bring it back for other tests / a clean end state.
	if err := Migrate(dsn); err != nil {
		t.Fatalf("migrate up (restore): %v", err)
	}
}

func TestSeedCategoriesPresent(t *testing.T) {
	st := freshStore(t)
	ctx := context.Background()

	cats, err := st.ListCategories(ctx)
	if err != nil {
		t.Fatalf("list categories: %v", err)
	}
	if len(cats) < 40 {
		t.Fatalf("expected the full seed set (40+ categories), got %d", len(cats))
	}

	// Spot-check a known parent and child relationship.
	movies, err := st.GetCategory(ctx, 2000)
	if err != nil {
		t.Fatalf("get category 2000: %v", err)
	}
	if movies.Name != "Movies" || movies.ParentID != nil {
		t.Errorf("category 2000 = %+v, want name Movies with nil parent", movies)
	}
	hd, err := st.GetCategory(ctx, 2040)
	if err != nil {
		t.Fatalf("get category 2040: %v", err)
	}
	if hd.ParentID == nil || *hd.ParentID != 2000 {
		t.Errorf("category 2040 parent = %v, want 2000", hd.ParentID)
	}

	if _, err := st.GetCategory(ctx, 999999); err != ErrNotFound {
		t.Errorf("expected ErrNotFound for unknown category, got %v", err)
	}
}

func TestGroupBackfillTarget(t *testing.T) {
	st := freshStore(t)
	ctx := context.Background()

	g, err := st.UpsertGroup(ctx, "alt.binaries.bftarget", true)
	if err != nil {
		t.Fatal(err)
	}
	// No target initially.
	if g.BackfillTargetDays != nil || g.BackfillTargetArticles != nil {
		t.Errorf("expected nil targets initially, got %+v", g)
	}
	has, err := st.AnyGroupHasBackfillTarget(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if has {
		t.Error("expected no group with backfill target")
	}

	// Set both dimensions.
	days := 30
	arts := int64(50000)
	if err := st.SetGroupBackfillTarget(ctx, g.ID, &days, &arts); err != nil {
		t.Fatal(err)
	}
	got, _ := st.GetGroupByName(ctx, g.Name)
	if got.BackfillTargetDays == nil || *got.BackfillTargetDays != 30 {
		t.Errorf("target days = %v, want 30", got.BackfillTargetDays)
	}
	if got.BackfillTargetArticles == nil || *got.BackfillTargetArticles != 50000 {
		t.Errorf("target articles = %v, want 50000", got.BackfillTargetArticles)
	}

	has, _ = st.AnyGroupHasBackfillTarget(ctx)
	if !has {
		t.Error("expected a group with backfill target after setting")
	}

	// Clear the days dimension (nil), keep articles.
	if err := st.SetGroupBackfillTarget(ctx, g.ID, nil, &arts); err != nil {
		t.Fatal(err)
	}
	got, _ = st.GetGroupByName(ctx, g.Name)
	if got.BackfillTargetDays != nil {
		t.Errorf("target days should be cleared, got %v", got.BackfillTargetDays)
	}
	if got.BackfillTargetArticles == nil {
		t.Error("target articles should remain set")
	}

	// Not found.
	if err := st.SetGroupBackfillTarget(ctx, 99999, &days, nil); err != ErrNotFound {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestGroupCRUD(t *testing.T) {
	st := freshStore(t)
	ctx := context.Background()

	g, err := st.UpsertGroup(ctx, "alt.binaries.test", true)
	if err != nil {
		t.Fatalf("upsert group: %v", err)
	}
	if g.ID == 0 || g.Name != "alt.binaries.test" || !g.Active {
		t.Fatalf("unexpected group: %+v", g)
	}

	// Upserting the same name returns the same row (idempotent).
	g2, err := st.UpsertGroup(ctx, "alt.binaries.test", true)
	if err != nil {
		t.Fatalf("re-upsert group: %v", err)
	}
	if g2.ID != g.ID {
		t.Fatalf("upsert created a duplicate: %d vs %d", g2.ID, g.ID)
	}

	// Position updates persist.
	if err := st.UpdateGroupForwardPosition(ctx, g.ID, 12345); err != nil {
		t.Fatalf("update forward: %v", err)
	}
	if err := st.UpdateGroupBackfillPosition(ctx, g.ID, 1000, false); err != nil {
		t.Fatalf("update backfill: %v", err)
	}
	got, err := st.GetGroupByName(ctx, "alt.binaries.test")
	if err != nil {
		t.Fatalf("get group: %v", err)
	}
	if got.LastScannedHigh != 12345 {
		t.Errorf("LastScannedHigh = %d, want 12345", got.LastScannedHigh)
	}
	if got.BackfillLow != 1000 || got.BackfillComplete {
		t.Errorf("backfill = (%d, %t), want (1000, false)", got.BackfillLow, got.BackfillComplete)
	}

	// Toggle active and list active-only.
	if err := st.SetGroupActive(ctx, g.ID, false); err != nil {
		t.Fatalf("set inactive: %v", err)
	}
	active, err := st.ListGroups(ctx, true)
	if err != nil {
		t.Fatalf("list active: %v", err)
	}
	if len(active) != 0 {
		t.Errorf("expected 0 active groups, got %d", len(active))
	}
	all, err := st.ListGroups(ctx, false)
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 1 {
		t.Errorf("expected 1 group total, got %d", len(all))
	}

	// Delete.
	if err := st.DeleteGroup(ctx, g.ID); err != nil {
		t.Fatalf("delete group: %v", err)
	}
	if _, err := st.GetGroupByName(ctx, "alt.binaries.test"); err != ErrNotFound {
		t.Errorf("expected ErrNotFound after delete, got %v", err)
	}
	if err := st.DeleteGroup(ctx, g.ID); err != ErrNotFound {
		t.Errorf("expected ErrNotFound deleting missing group, got %v", err)
	}
}

func TestPipelineStatistics(t *testing.T) {
	st := freshStore(t)
	ctx := context.Background()
	g, _ := st.UpsertGroup(ctx, "alt.binaries.stats", true)

	// Seed a complete single-file binary (5/5) and an incomplete one (2/4).
	seedStatsParts(t, st, g.ID, "Complete.mkv", "p1", 1000, 5, 5)
	seedStatsParts(t, st, g.ID, "Incomplete.mkv", "p2", 2000, 2, 4)
	if _, err := st.AssembleBinaries(ctx, 100); err != nil {
		t.Fatal(err)
	}

	stats, err := st.PipelineStatistics(ctx)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	// Two binaries formed, one complete.
	if stats.BinariesTotal != 2 {
		t.Errorf("binaries total = %d, want 2", stats.BinariesTotal)
	}
	if stats.BinariesComplete != 1 {
		t.Errorf("binaries complete = %d, want 1", stats.BinariesComplete)
	}
	// The complete one is unreleased (no release builder run here).
	if stats.BinariesUnreleased != 1 {
		t.Errorf("binaries unreleased = %d, want 1", stats.BinariesUnreleased)
	}
	// ReleasesByPP is initialised even with no releases.
	if stats.ReleasesByPP == nil {
		t.Error("ReleasesByPP should be non-nil")
	}
	if stats.ReleasesTotal != 0 {
		t.Errorf("releases total = %d, want 0 (no build run)", stats.ReleasesTotal)
	}
	// Estimates are non-negative (exact values depend on ANALYZE timing).
	if stats.PartsTotal < 0 || stats.PartsUnassigned < 0 {
		t.Errorf("estimates must be non-negative: %+v", stats)
	}

	// Seed two failed releases: one still retryable, one that has exhausted its
	// retry budget. Only the exhausted one should be counted.
	c := 5000
	for _, rel := range []struct {
		guid     string
		attempts int
	}{
		{"stats-fail-retryable", 1},
		{"stats-fail-exhausted", MaxPPAttempts},
	} {
		if _, _, err := st.CreateRelease(ctx, ReleaseInput{
			GUID: rel.guid, Name: rel.guid, SearchName: rel.guid, CategoryID: &c, ReleaseHash: rel.guid,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := st.Pool().Exec(ctx,
			`UPDATE releases SET pp_status='failed', pp_attempts=$2 WHERE guid=$1`, rel.guid, rel.attempts); err != nil {
			t.Fatal(err)
		}
	}
	stats2, err := st.PipelineStatistics(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats2.ReleasesByPP["failed"] != 2 {
		t.Errorf("failed = %d, want 2", stats2.ReleasesByPP["failed"])
	}
	if stats2.ReleasesFailedExhausted != 1 {
		t.Errorf("failed exhausted = %d, want 1 (only the attempts>=max one)", stats2.ReleasesFailedExhausted)
	}

	// Requeue the failed releases: both should go back to pending with attempts=0.
	n, err := st.RequeueFailedReleases(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("requeued = %d, want 2", n)
	}
	stats3, err := st.PipelineStatistics(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats3.ReleasesByPP["failed"] != 0 {
		t.Errorf("failed after requeue = %d, want 0", stats3.ReleasesByPP["failed"])
	}
	if stats3.ReleasesFailedExhausted != 0 {
		t.Errorf("exhausted after requeue = %d, want 0", stats3.ReleasesFailedExhausted)
	}
	var attempts int
	if err := st.Pool().QueryRow(ctx,
		`SELECT coalesce(max(pp_attempts),0) FROM releases WHERE guid IN ('stats-fail-retryable','stats-fail-exhausted')`).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if attempts != 0 {
		t.Errorf("pp_attempts after requeue = %d, want 0", attempts)
	}

	// Per-group breakdown: create two releases in group g (one pending) and
	// verify the Groups slice reflects them.
	gid := g.ID
	for i, st2 := range []string{"pending", "done"} {
		guid := fmt.Sprintf("grp-rel-%d", i)
		if _, _, err := st.CreateRelease(ctx, ReleaseInput{
			GUID: guid, Name: guid, SearchName: guid, GroupID: &gid, CategoryID: &c, ReleaseHash: guid,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := st.Pool().Exec(ctx, `UPDATE releases SET pp_status=$2 WHERE guid=$1`, guid, st2); err != nil {
			t.Fatal(err)
		}
	}
	stats4, err := st.PipelineStatistics(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var found *GroupReleaseStats
	for i := range stats4.Groups {
		if stats4.Groups[i].Name == "alt.binaries.stats" {
			found = &stats4.Groups[i]
		}
	}
	if found == nil {
		t.Fatal("expected per-group stats for alt.binaries.stats")
	}
	if found.ReleasesTotal != 2 || found.ReleasesPending != 1 {
		t.Errorf("group stats = %+v, want total=2 pending=1", *found)
	}
}

// seedStatsParts inserts `collected` of `total` parts for a single-file binary.
func seedStatsParts(t *testing.T, st *Store, groupID int64, norm, poster string, articleBase int64, collected, total int) {
	t.Helper()
	var parts []PartInput
	for i := 1; i <= collected; i++ {
		parts = append(parts, PartInput{
			GroupID:       groupID,
			ArticleNumber: articleBase + int64(i),
			MessageID:     fmt.Sprintf("m-%s-%d@x", norm, i),
			Subject:       fmt.Sprintf(`"%s" yEnc (%d/%d)`, norm, i, total),
			Poster:        poster,
			Bytes:         1000,
			PartNumber:    i,
			TotalParts:    total,
			NormSubject:   norm,
		})
	}
	if _, err := st.InsertParts(context.Background(), parts); err != nil {
		t.Fatalf("seed stats parts: %v", err)
	}
}
