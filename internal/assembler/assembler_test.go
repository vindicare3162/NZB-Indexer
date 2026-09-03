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
		{0, 0, false}, // nothing collected -> not complete
		{1, 0, true},  // one part, no declared total -> single-article file, complete
		{5, 0, true},  // parts present, no declared total -> complete
		{4, 5, false}, // missing one
		{5, 5, true},  // exact
		{6, 5, true},  // more than declared (duplicates) still complete
	}
	for _, c := range cases {
		if got := IsComplete(c.collected, c.total); got != c.want {
			t.Errorf("IsComplete(%d,%d) = %v, want %v", c.collected, c.total, got, c.want)
		}
	}
}

// drainRepo is a mock Repo that returns a scripted sequence of touched counts
// from AssembleBinaries, for testing the drain loop without a database.
type drainRepo struct {
	touchedSeq []int // returned in order; missing entries return 0
	calls      int
	aged       int64
}

func (d *drainRepo) AssembleBinaries(_ context.Context, _ int) (int, error) {
	i := d.calls
	d.calls++
	if i < len(d.touchedSeq) {
		return d.touchedSeq[i], nil
	}
	return 0, nil
}
func (d *drainRepo) AgeOutStaleBinaries(_ context.Context, _ time.Duration) (int64, error) {
	return d.aged, nil
}
func (d *drainRepo) ListCompleteUnreleasedBinaries(_ context.Context, _ int) ([]store.Binary, error) {
	return nil, nil
}

func TestAssembleDrainsBacklog(t *testing.T) {
	// Three full-ish batches then nothing: the loop should run until a batch
	// touches 0 and report the backlog fully drained.
	repo := &drainRepo{touchedSeq: []int{500, 500, 120}}
	a := New(repo, nil, Options{BatchLimit: 500, MaxBatchesPerRun: 100})

	res, err := a.Assemble(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.BinariesTouched != 1120 {
		t.Errorf("BinariesTouched = %d, want 1120", res.BinariesTouched)
	}
	if res.Batches != 3 {
		t.Errorf("Batches = %d, want 3", res.Batches)
	}
	if !res.Drained {
		t.Error("expected Drained = true when a batch touches 0")
	}
	// 4 calls: 3 productive + 1 that returned 0 to stop.
	if repo.calls != 4 {
		t.Errorf("AssembleBinaries calls = %d, want 4", repo.calls)
	}
}

func TestAssembleRespectsBatchCap(t *testing.T) {
	// Every batch returns a full count; the cap must stop the loop and report
	// not-drained.
	repo := &drainRepo{touchedSeq: []int{500, 500, 500, 500, 500}}
	a := New(repo, nil, Options{BatchLimit: 500, MaxBatchesPerRun: 3})

	res, err := a.Assemble(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Batches != 3 {
		t.Errorf("Batches = %d, want 3 (capped)", res.Batches)
	}
	if res.BinariesTouched != 1500 {
		t.Errorf("BinariesTouched = %d, want 1500", res.BinariesTouched)
	}
	if res.Drained {
		t.Error("expected Drained = false when the cap is hit")
	}
	if repo.calls != 3 {
		t.Errorf("AssembleBinaries calls = %d, want 3 (should not exceed cap)", repo.calls)
	}
}

func TestAssembleHonoursContextCancel(t *testing.T) {
	repo := &drainRepo{touchedSeq: []int{500, 500, 500}}
	a := New(repo, nil, Options{BatchLimit: 500, MaxBatchesPerRun: 100})
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	_, err := a.Assemble(ctx)
	if err == nil {
		t.Error("expected context cancellation error")
	}
	if repo.calls != 0 {
		t.Errorf("should not run any batch when ctx is already cancelled, got %d calls", repo.calls)
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

// seedCollectionFile inserts the segments of one file within a multi-file
// collection. All files of a collection share collectionKey and collectionFiles;
// fileNum is this file's 1-based position.
func seedCollectionFile(t *testing.T, st *store.Store, groupID int64, collectionKey string, collectionFiles, fileNum int, fileName, poster string, articleBase int64, segTotal int) {
	t.Helper()
	var parts []store.PartInput
	for seg := 1; seg <= segTotal; seg++ {
		parts = append(parts, store.PartInput{
			GroupID:         groupID,
			ArticleNumber:   articleBase + int64(seg),
			MessageID:       fmt.Sprintf("m-%s-f%d-s%d@x", collectionKey, fileNum, seg),
			Subject:         fmt.Sprintf(`[%d/%d] "%s" yEnc (%d/%d)`, fileNum, collectionFiles, fileName, seg, segTotal),
			Poster:          poster,
			Bytes:           1000,
			PartNumber:      seg,
			TotalParts:      segTotal,
			NormSubject:     fmt.Sprintf(`[%d/%d] "%s"`, fileNum, collectionFiles, fileName),
			CollectionKey:   collectionKey,
			FileNumber:      fileNum,
			CollectionFiles: collectionFiles,
		})
	}
	if _, err := st.InsertParts(context.Background(), parts); err != nil {
		t.Fatalf("seed collection file: %v", err)
	}
}

// TestAssembleCollectionGroupsIntoOneBinary is the core #18 guarantee: a
// multi-file "[n/total]" post (rar volumes + PAR2) folds into ONE binary whose
// completeness is judged by files present, not per-file segment counts. The
// segments of the whole collection end up under one binary_id so downstream
// post-processing/NZB see the PAR2 alongside the rar volumes.
func TestAssembleCollectionGroupsIntoOneBinary(t *testing.T) {
	st := freshStore(t)
	ctx := context.Background()
	g, _ := st.UpsertGroup(ctx, "alt.binaries.coll", true)

	// A 3-file collection: a PAR2 and two rar volumes, all keyed "Obf/3".
	const key = "Obf/3"
	seedCollectionFile(t, st, g.ID, key, 3, 1, "Obf.par2", "p", 1000, 2)
	seedCollectionFile(t, st, g.ID, key, 3, 2, "Obf.part1.rar", "p", 2000, 5)
	// Only 2 of 3 files so far -> incomplete.
	a := New(st, nil, Options{BatchLimit: 100})
	if _, err := a.Assemble(ctx); err != nil {
		t.Fatal(err)
	}
	complete, _ := st.ListCompleteUnreleasedBinaries(ctx, 100)
	if len(complete) != 0 {
		t.Fatalf("collection should be incomplete with 2/3 files, got %d complete", len(complete))
	}

	// The third file arrives in a later scan; the collection completes.
	seedCollectionFile(t, st, g.ID, key, 3, 3, "Obf.part2.rar", "p", 3000, 5)
	if _, err := a.Assemble(ctx); err != nil {
		t.Fatal(err)
	}
	complete, _ = st.ListCompleteUnreleasedBinaries(ctx, 100)
	if len(complete) != 1 {
		t.Fatalf("collection should be complete with 3/3 files, got %d", len(complete))
	}
	b := complete[0]
	if b.CollectionKey != key || b.CollectionFiles != 3 {
		t.Errorf("binary collection = %q/%d, want %q/3", b.CollectionKey, b.CollectionFiles, key)
	}
	if b.CollectedParts != 3 {
		t.Errorf("collected files = %d, want 3", b.CollectedParts)
	}

	// All 12 segments (2+5+5) of the whole collection share one binary_id.
	var segs int
	if err := st.Pool().QueryRow(ctx,
		`SELECT count(*) FROM parts WHERE binary_id = $1`, b.ID).Scan(&segs); err != nil {
		t.Fatal(err)
	}
	if segs != 12 {
		t.Errorf("segments under collection binary = %d, want 12", segs)
	}

	// No unassigned parts remain.
	var unassigned int
	st.Pool().QueryRow(ctx, `SELECT count(*) FROM parts WHERE binary_id IS NULL`).Scan(&unassigned)
	if unassigned != 0 {
		t.Errorf("unassigned parts = %d, want 0", unassigned)
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
	// Single-article file: one part, no declared total (no yEnc counter) -> a
	// complete single-file binary (issue #28).
	seedParts(t, st, g.ID, "Single.Article.File", "poster3", 3000, []int{1}, 0)

	a := New(st, nil, Options{BatchLimit: 100})
	res, err := a.Assemble(ctx)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	if res.BinariesTouched != 3 {
		t.Errorf("BinariesTouched = %d, want 3", res.BinariesTouched)
	}

	// Complete binaries: the full 5/5 set and the single-article file.
	complete, err := st.ListCompleteUnreleasedBinaries(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(complete) != 2 {
		t.Fatalf("complete binaries = %d, want 2 (5/5 set + single-article)", len(complete))
	}
	byName := map[string]store.Binary{}
	for _, b := range complete {
		byName[b.NormSubject] = b
	}
	full, ok := byName["Complete.Release.mkv"]
	if !ok {
		t.Fatal("expected Complete.Release.mkv to be complete")
	}
	if full.CollectedParts != 5 || full.TotalParts != 5 {
		t.Errorf("full binary parts = %d/%d, want 5/5", full.CollectedParts, full.TotalParts)
	}
	single, ok := byName["Single.Article.File"]
	if !ok {
		t.Fatal("expected Single.Article.File (total_parts=0, 1 part) to be complete")
	}
	if single.CollectedParts != 1 || single.TotalParts != 0 {
		t.Errorf("single-article binary = %d/%d, want 1/0", single.CollectedParts, single.TotalParts)
	}
	if byName["Incomplete.Release.mkv"].Complete {
		t.Error("Incomplete.Release.mkv (2/4) must not be complete")
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
