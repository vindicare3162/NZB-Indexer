package release

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/vindicare/goindex/internal/store"
)

func freshStore(t *testing.T) *store.Store {
	t.Helper()
	dsn := os.Getenv("GOINDEX_TEST_DSN")
	if dsn == "" {
		t.Skip("GOINDEX_TEST_DSN not set; skipping release integration test")
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

// seedCompleteBinary inserts parts for a complete binary and assembles it, so
// it appears in ListCompleteUnreleasedBinaries.
func seedCompleteBinary(t *testing.T, st *store.Store, groupID int64, norm, poster string, base int64, total int) {
	t.Helper()
	var parts []store.PartInput
	for i := 1; i <= total; i++ {
		parts = append(parts, store.PartInput{
			GroupID:       groupID,
			ArticleNumber: base + int64(i),
			MessageID:     fmt.Sprintf("m-%s-%d@x", norm, i),
			Subject:       fmt.Sprintf(`"%s" yEnc (%d/%d)`, norm, i, total),
			Poster:        poster,
			Bytes:         1_000_000,
			PartNumber:    i,
			TotalParts:    total,
			NormSubject:   norm,
		})
	}
	if _, err := st.InsertParts(context.Background(), parts); err != nil {
		t.Fatalf("insert parts: %v", err)
	}
	if _, err := st.AssembleBinaries(context.Background(), 100); err != nil {
		t.Fatalf("assemble: %v", err)
	}
}

func TestBuildCreatesReleasesWithCategoryAndDedup(t *testing.T) {
	st := freshStore(t)
	ctx := context.Background()
	g, _ := st.UpsertGroup(ctx, "alt.binaries.rel", true)

	// A TV release and a movie release.
	seedCompleteBinary(t, st, g.ID, "Some.Show.S01E01.1080p.WEB.x264-GRP.mkv", "p1", 1000, 3)
	seedCompleteBinary(t, st, g.ID, "Great.Movie.2024.1080p.BluRay.x264-GRP.mkv", "p2", 2000, 4)

	b := New(st, nil, Options{BatchLimit: 100})
	res, err := b.Build(ctx)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if res.Created != 2 {
		t.Errorf("Created = %d, want 2", res.Created)
	}

	total, _ := st.CountReleases(ctx)
	if total != 2 {
		t.Fatalf("releases = %d, want 2", total)
	}

	// Verify category assignment via a direct query.
	rows, err := st.Pool().Query(ctx,
		`SELECT name, category_id FROM releases ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string]int{}
	for rows.Next() {
		var name string
		var cat *int
		if err := rows.Scan(&name, &cat); err != nil {
			t.Fatal(err)
		}
		if cat != nil {
			got[name] = *cat
		}
	}
	if got["Great.Movie.2024.1080p.BluRay.x264-GRP"] != CatMoviesHD {
		t.Errorf("movie category = %d, want %d (Movies HD)", got["Great.Movie.2024.1080p.BluRay.x264-GRP"], CatMoviesHD)
	}
	if got["Some.Show.S01E01.1080p.WEB.x264-GRP"] != CatTVHD {
		t.Errorf("tv category = %d, want %d (TV HD)", got["Some.Show.S01E01.1080p.WEB.x264-GRP"], CatTVHD)
	}

	// Binaries should now be marked released.
	remaining, _ := st.ListCompleteUnreleasedBinaries(ctx, 100)
	if len(remaining) != 0 {
		t.Errorf("unreleased complete binaries = %d, want 0", len(remaining))
	}

	// Re-running build creates nothing new.
	res2, err := b.Build(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res2.Created != 0 || res2.Processed != 0 {
		t.Errorf("second build = %+v, want no work", res2)
	}
}

func TestBuildDedupCollapsesRepost(t *testing.T) {
	st := freshStore(t)
	ctx := context.Background()
	g, _ := st.UpsertGroup(ctx, "alt.binaries.dup", true)

	// Two separate binaries representing the same content reposted (same name
	// and size, different poster/articles => different binary rows).
	seedCompleteBinary(t, st, g.ID, "Great.Movie.2024.1080p.BluRay.x264-GRP.mkv", "poster-a", 1000, 4)
	seedCompleteBinary(t, st, g.ID, "Great.Movie.2024.1080p.BluRay.x264-GRP.mkv", "poster-b", 5000, 4)

	b := New(st, nil, Options{BatchLimit: 100})
	res, err := b.Build(ctx)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if res.Processed != 2 {
		t.Errorf("Processed = %d, want 2", res.Processed)
	}
	if res.Created != 1 || res.Duplicates != 1 {
		t.Errorf("Created/Duplicates = %d/%d, want 1/1", res.Created, res.Duplicates)
	}

	total, _ := st.CountReleases(ctx)
	if total != 1 {
		t.Errorf("releases = %d, want 1 (duplicate collapsed)", total)
	}
}
