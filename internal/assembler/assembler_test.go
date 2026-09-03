package assembler

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/vindicare/goindex/internal/store"
)

func TestIsComplete(t *testing.T) {
	cases := []struct {
		collected, total int
		want             bool
	}{
		{0, 0, false},  // nothing known
		{5, 0, false},  // total unknown -> never complete
		{4, 5, false},  // missing one
		{5, 5, true},   // exact
		{6, 5, true},   // more than declared (duplicates) still complete
	}
	for _, c := range cases {
		if got := IsComplete(c.collected, c.total); got != c.want {
			t.Errorf("IsComplete(%d,%d) = %v, want %v", c.collected, c.total, got, c.want)
		}
	}
}

func freshStore(t *testing.T) *store.Store {
	t.Helper()
	dsn := os.Getenv("GOINDEX_TEST_DSN")
	if dsn == "" {
		t.Skip("GOINDEX_TEST_DSN not set; skipping assembler integration test")
	}
	if err := store.MigrateDown(dsn); err != nil {
		t.Fatalf("migrate down: %v", err)
	}
	if err := store.Migrate(dsn); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st, err := store.Open(ctx, dsn, 5)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)
	return st
}

// seedParts inserts count parts for one binary keyed by norm under the given
// group/poster. Part numbers can be provided out of order.
func seedParts(t *testing.T, st *store.Store, groupID int64, norm, poster string, articleBase int64, partNums []int, total int) {
	t.Helper()
	var parts []store.PartInput
	for i, pn := range partNums {
		parts = append(parts, store.PartInput{
			GroupID:       groupID,
			ArticleNumber: articleBase + int64(i),
			MessageID:     fmt.Sprintf("m-%s-%d@x", norm, pn),
			Subject:       fmt.Sprintf(`"%s" yEnc (%d/%d)`, norm, pn, total),
			Poster:        poster,
			Bytes:         1000,
			PartNumber:    pn,
			TotalParts:    total,
			NormSubject:   norm,
		})
	}
	if _, err := st.InsertParts(context.Background(), parts); err != nil {
		t.Fatalf("seed parts: %v", err)
	}
}

func TestAssembleGroupsAndCompletion(t *testing.T) {
	st := freshStore(t)
	ctx := context.Background()
	g, _ := st.UpsertGroup(ctx, "alt.binaries.asm", true)

	// Complete binary: all 5 parts, arriving out of order.
	seedParts(t, st, g.ID, "Complete.Release.mkv", "poster1", 1000, []int{3, 1, 5, 2, 4}, 5)
	// Incomplete binary: 2 of 4 parts.
	seedParts(t, st, g.ID, "Incomplete.Release.mkv", "poster2", 2000, []int{1, 3}, 4)
	// Unknown-total binary: parts present but total_parts = 0 -> never complete.
	seedParts(t, st, g.ID, "Unknown.Total.file", "poster3", 3000, []int{0, 0}, 0)

	a := New(st, nil, Options{BatchLimit: 100})
	res, err := a.Assemble(ctx)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	if res.BinariesTouched != 3 {
		t.Errorf("BinariesTouched = %d, want 3", res.BinariesTouched)
	}

	// Complete binaries should be exactly the one full 5/5 set.
	complete, err := st.ListCompleteUnreleasedBinaries(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(complete) != 1 {
		t.Fatalf("complete binaries = %d, want 1", len(complete))
	}
	b := complete[0]
	if b.NormSubject != "Complete.Release.mkv" {
		t.Errorf("complete binary = %q, want Complete.Release.mkv", b.NormSubject)
	}
	if b.CollectedParts != 5 || b.TotalParts != 5 {
		t.Errorf("complete binary parts = %d/%d, want 5/5", b.CollectedParts, b.TotalParts)
	}
	if b.TotalBytes != 5000 {
		t.Errorf("total bytes = %d, want 5000", b.TotalBytes)
	}

	// All parts should now be linked to a binary (none left unassigned).
	var unassigned int
	if err := st.Pool().QueryRow(ctx,
		`SELECT count(*) FROM parts WHERE binary_id IS NULL`).Scan(&unassigned); err != nil {
		t.Fatal(err)
	}
	if unassigned != 0 {
		t.Errorf("unassigned parts = %d, want 0", unassigned)
	}
}

func TestAssembleAccumulatesAcrossScans(t *testing.T) {
	st := freshStore(t)
	ctx := context.Background()
	g, _ := st.UpsertGroup(ctx, "alt.binaries.accum", true)

	// First scan delivers 3 of 5 parts; assemble -> incomplete.
	seedParts(t, st, g.ID, "Split.Release.mkv", "p", 1000, []int{1, 2, 3}, 5)
	a := New(st, nil, Options{BatchLimit: 100})
	if _, err := a.Assemble(ctx); err != nil {
		t.Fatal(err)
	}
	complete, _ := st.ListCompleteUnreleasedBinaries(ctx, 100)
	if len(complete) != 0 {
		t.Fatalf("expected no complete binary after partial scan, got %d", len(complete))
	}

	// Second scan delivers the remaining 2 parts; assemble should accumulate
	// and mark the binary complete.
	seedParts(t, st, g.ID, "Split.Release.mkv", "p", 2000, []int{4, 5}, 5)
	if _, err := a.Assemble(ctx); err != nil {
		t.Fatal(err)
	}
	complete, _ = st.ListCompleteUnreleasedBinaries(ctx, 100)
	if len(complete) != 1 {
		t.Fatalf("expected 1 complete binary after second scan, got %d", len(complete))
	}
	if complete[0].CollectedParts != 5 {
		t.Errorf("collected = %d, want 5", complete[0].CollectedParts)
	}
}

func TestAgeOutStaleBinaries(t *testing.T) {
	st := freshStore(t)
	ctx := context.Background()
	g, _ := st.UpsertGroup(ctx, "alt.binaries.stale", true)

	// One incomplete and one complete binary.
	seedParts(t, st, g.ID, "Stale.Incomplete.mkv", "p1", 1000, []int{1, 2}, 5)
	seedParts(t, st, g.ID, "Fresh.Complete.mkv", "p2", 2000, []int{1, 2, 3}, 3)
	a := New(st, nil, Options{BatchLimit: 100})
	if _, err := a.Assemble(ctx); err != nil {
		t.Fatal(err)
	}

	// Force the incomplete binary to look old.
	if _, err := st.Pool().Exec(ctx,
		`UPDATE binaries SET updated_at = now() - interval '10 days' WHERE norm_subject = 'Stale.Incomplete.mkv'`); err != nil {
		t.Fatal(err)
	}

	removed, err := st.AgeOutStaleBinaries(ctx, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("age out: %v", err)
	}
	if removed != 1 {
		t.Errorf("removed = %d, want 1", removed)
	}

	// The complete binary must survive; the stale one and its parts are gone.
	var bins, staleParts int
	st.Pool().QueryRow(ctx, `SELECT count(*) FROM binaries`).Scan(&bins)
	st.Pool().QueryRow(ctx,
		`SELECT count(*) FROM parts WHERE norm_subject = 'Stale.Incomplete.mkv'`).Scan(&staleParts)
	if bins != 1 {
		t.Errorf("remaining binaries = %d, want 1", bins)
	}
	if staleParts != 0 {
		t.Errorf("stale parts remaining = %d, want 0", staleParts)
	}
}
