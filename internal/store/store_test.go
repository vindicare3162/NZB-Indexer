package store

import (
	"context"
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
	if v != 3 {
		t.Fatalf("expected schema version 3, got %d", v)
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
