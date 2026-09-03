package scanner

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/vindicare/goindex/internal/nntp"
	"github.com/vindicare/goindex/internal/store"
)

// fakeSource is an in-memory NNTP source backed by a fixed article list.
type fakeSource struct {
	info      nntp.GroupInfo
	articles  map[int64]nntp.Overview // keyed by article number
	overCalls int
}

func (f *fakeSource) SelectGroupInfo(_ context.Context, group string) (nntp.GroupInfo, error) {
	gi := f.info
	gi.Name = group
	return gi, nil
}

func (f *fakeSource) Overview(_ context.Context, _ string, begin, end int64) ([]nntp.Overview, error) {
	f.overCalls++
	var out []nntp.Overview
	for n := begin; n <= end; n++ {
		if a, ok := f.articles[n]; ok {
			a.ArticleNumber = n
			out = append(out, a)
		}
	}
	return out, nil
}

// buildSource fabricates a group of `count` segments of one binary, article
// numbers [low..low+count-1], each posted `agePerArticle` apart ending now.
func buildSource(low, count int64, agePerArticle time.Duration) *fakeSource {
	arts := make(map[int64]nntp.Overview, count)
	now := time.Now().UTC().Truncate(time.Second)
	for i := int64(0); i < count; i++ {
		n := low + i
		part := i + 1
		arts[n] = nntp.Overview{
			Subject:   fmt.Sprintf(`"Test.Release.2024.1080p.mkv" yEnc (%d/%d)`, part, count),
			From:      "poster@example.com",
			Date:      now.Add(-time.Duration(count-i) * agePerArticle),
			MessageID: fmt.Sprintf("msg-%d@example.com", n),
			Bytes:     500000,
		}
	}
	return &fakeSource{
		info:     nntp.GroupInfo{Low: low, High: low + count - 1, Count: count},
		articles: arts,
	}
}

func freshStore(t *testing.T) *store.Store {
	t.Helper()
	dsn := os.Getenv("GOINDEX_TEST_DSN")
	if dsn == "" {
		t.Skip("GOINDEX_TEST_DSN not set; skipping scanner integration test")
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

func TestScanForwardIngestsAndResumes(t *testing.T) {
	st := freshStore(t)
	ctx := context.Background()

	g, err := st.UpsertGroup(ctx, "alt.binaries.scan", true)
	if err != nil {
		t.Fatal(err)
	}

	// 250 articles starting at 1000; small batch size to force multiple XOVER
	// calls and exercise watermark persistence between batches.
	src := buildSource(1000, 250, time.Minute)
	sc := New(src, st, nil, Options{BatchSize: 100})

	res, err := sc.ScanForward(ctx, g.Name)
	if err != nil {
		t.Fatalf("scan forward: %v", err)
	}
	if res.PartsInserted != 250 {
		t.Errorf("PartsInserted = %d, want 250", res.PartsInserted)
	}
	if res.NewHigh != 1249 {
		t.Errorf("NewHigh = %d, want 1249", res.NewHigh)
	}

	got, err := st.CountParts(ctx, g.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got != 250 {
		t.Errorf("stored parts = %d, want 250", got)
	}

	// Watermark should have advanced.
	g2, err := st.GetGroupByName(ctx, g.Name)
	if err != nil {
		t.Fatal(err)
	}
	if g2.LastScannedHigh != 1249 {
		t.Errorf("LastScannedHigh = %d, want 1249", g2.LastScannedHigh)
	}

	// Re-scan with the same data: nothing new should be inserted (idempotent
	// resume). Add 50 more articles at the top to confirm only those are new.
	src2 := buildSource(1000, 300, time.Minute) // now 1000..1299
	src2.info.High = 1299
	sc2 := New(src2, st, nil, Options{BatchSize: 100})
	res2, err := sc2.ScanForward(ctx, g.Name)
	if err != nil {
		t.Fatalf("second scan: %v", err)
	}
	if res2.PartsInserted != 50 {
		t.Errorf("second scan inserted = %d, want 50 (only the new tail)", res2.PartsInserted)
	}
	total, _ := st.CountParts(ctx, g.ID)
	if total != 300 {
		t.Errorf("total parts = %d, want 300", total)
	}
}

func TestScanForwardParsesPartMetadata(t *testing.T) {
	st := freshStore(t)
	ctx := context.Background()
	g, _ := st.UpsertGroup(ctx, "alt.binaries.meta", true)

	src := buildSource(1, 5, time.Minute)
	sc := New(src, st, nil, Options{BatchSize: 100})
	if _, err := sc.ScanForward(ctx, g.Name); err != nil {
		t.Fatal(err)
	}

	// Inspect a stored part directly for parsed metadata.
	var partNum, totalParts int
	var norm string
	err := st.Pool().QueryRow(ctx,
		`SELECT part_number, total_parts, norm_subject FROM parts WHERE group_id=$1 AND article_number=3`,
		g.ID).Scan(&partNum, &totalParts, &norm)
	if err != nil {
		t.Fatal(err)
	}
	if partNum != 3 || totalParts != 5 {
		t.Errorf("part 3: got (%d/%d), want (3/5)", partNum, totalParts)
	}
	if norm != `"Test.Release.2024.1080p.mkv"` {
		t.Errorf("norm_subject = %q", norm)
	}
}

func TestScanBackfillWalksBackwardWithArticleLimit(t *testing.T) {
	st := freshStore(t)
	ctx := context.Background()
	g, _ := st.UpsertGroup(ctx, "alt.binaries.backfill", true)

	// Server has articles 1..1000. Simulate that forward scan already ingested
	// from 900 up, so backfill should walk 899 downward.
	src := buildSource(1, 1000, time.Minute)
	if err := st.UpdateGroupForwardPosition(ctx, g.ID, 1000); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateGroupBackfillPosition(ctx, g.ID, 900, false); err != nil {
		t.Fatal(err)
	}

	// Limit backfill to 200 articles this pass.
	sc := New(src, st, nil, Options{BatchSize: 100, BackfillMaxArticles: 200})
	res, err := sc.ScanBackfill(ctx, g.Name)
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if res.BackfillDone {
		t.Error("backfill should not be complete after a limited pass")
	}
	// Walked from 899 down to 700 (200 articles): new low = 700.
	if res.NewLow != 700 {
		t.Errorf("NewLow = %d, want 700", res.NewLow)
	}
	if res.PartsInserted != 200 {
		t.Errorf("PartsInserted = %d, want 200", res.PartsInserted)
	}

	g2, _ := st.GetGroupByName(ctx, g.Name)
	if g2.BackfillLow != 700 || g2.BackfillComplete {
		t.Errorf("backfill position = (%d, done=%t), want (700, false)", g2.BackfillLow, g2.BackfillComplete)
	}
}

func TestScanBackfillPerGroupTargetOverridesGlobal(t *testing.T) {
	st := freshStore(t)
	ctx := context.Background()
	g, _ := st.UpsertGroup(ctx, "alt.binaries.pgtarget", true)

	// Server has 1..1000; already ingested from 900 up.
	src := buildSource(1, 1000, time.Minute)
	if err := st.UpdateGroupForwardPosition(ctx, g.ID, 1000); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateGroupBackfillPosition(ctx, g.ID, 900, false); err != nil {
		t.Fatal(err)
	}
	// Per-group target: 50 articles per pass, overriding the global 200.
	target := int64(50)
	if err := st.SetGroupBackfillTarget(ctx, g.ID, nil, &target); err != nil {
		t.Fatal(err)
	}

	sc := New(src, st, nil, Options{BatchSize: 100, BackfillMaxArticles: 200})
	res, err := sc.ScanBackfill(ctx, g.Name)
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	// With the per-group cap of 50, walk from 899 down to 850.
	if res.NewLow != 850 {
		t.Errorf("NewLow = %d, want 850 (per-group target of 50 should win over global 200)", res.NewLow)
	}
	if res.PartsInserted != 50 {
		t.Errorf("PartsInserted = %d, want 50", res.PartsInserted)
	}
}

func TestScanBackfillCompletesAtServerLow(t *testing.T) {
	st := freshStore(t)
	ctx := context.Background()
	g, _ := st.UpsertGroup(ctx, "alt.binaries.bfdone", true)

	src := buildSource(1, 150, time.Minute) // articles 1..150
	if err := st.UpdateGroupBackfillPosition(ctx, g.ID, 100, false); err != nil {
		t.Fatal(err)
	}

	sc := New(src, st, nil, Options{BatchSize: 100})
	res, err := sc.ScanBackfill(ctx, g.Name)
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if !res.BackfillDone {
		t.Error("backfill should be complete when it reaches the server low")
	}
	g2, _ := st.GetGroupByName(ctx, g.Name)
	if !g2.BackfillComplete {
		t.Error("group backfill_complete should be true")
	}
}

// TestScanForwardRespectsArticleCap verifies that ForwardMaxArticles bounds a
// single forward pass so a firehose group yields, persisting the watermark so
// the next pass resumes where this one stopped.
func TestScanForwardRespectsArticleCap(t *testing.T) {
	st := freshStore(t)
	ctx := context.Background()

	g, err := st.UpsertGroup(ctx, "alt.binaries.firehose", true)
	if err != nil {
		t.Fatal(err)
	}

	// Server has 500 articles at 1000..1499. First scan is bootstrap
	// (LastScannedHigh == 0), so it starts at the server low (1000) and the cap
	// of 200 should stop it at 1199.
	src := buildSource(1000, 500, time.Minute)
	sc := New(src, st, nil, Options{BatchSize: 50, ForwardMaxArticles: 200})

	res, err := sc.ScanForward(ctx, g.Name)
	if err != nil {
		t.Fatalf("scan forward: %v", err)
	}
	if res.PartsInserted != 200 {
		t.Errorf("first pass PartsInserted = %d, want 200 (capped)", res.PartsInserted)
	}

	g2, err := st.GetGroupByName(ctx, g.Name)
	if err != nil {
		t.Fatal(err)
	}
	if g2.LastScannedHigh != 1199 {
		t.Errorf("watermark after capped pass = %d, want 1199", g2.LastScannedHigh)
	}

	// Second pass resumes at 1200 and, still capped at 200, stops at 1399.
	res2, err := sc.ScanForward(ctx, g.Name)
	if err != nil {
		t.Fatalf("second scan: %v", err)
	}
	if res2.PartsInserted != 200 {
		t.Errorf("second pass PartsInserted = %d, want 200 (capped, resumed)", res2.PartsInserted)
	}
	g3, _ := st.GetGroupByName(ctx, g.Name)
	if g3.LastScannedHigh != 1399 {
		t.Errorf("watermark after second pass = %d, want 1399", g3.LastScannedHigh)
	}

	// Third pass ingests the remaining 100 (1400..1499); cap is not reached.
	res3, err := sc.ScanForward(ctx, g.Name)
	if err != nil {
		t.Fatalf("third scan: %v", err)
	}
	if res3.PartsInserted != 100 {
		t.Errorf("third pass PartsInserted = %d, want 100 (tail)", res3.PartsInserted)
	}
	total, _ := st.CountParts(ctx, g.ID)
	if total != 500 {
		t.Errorf("total parts = %d, want 500", total)
	}
}
