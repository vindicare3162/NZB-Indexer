package store

import (
	"context"
	"testing"
)

// mkRelease inserts a release with the given name; search_name mirrors the
// normalization the release builder uses (lowercased, separators -> spaces).
func mkRelease(t *testing.T, st *Store, guid, name, searchName string, cat int) {
	t.Helper()
	c := cat
	_, _, err := st.CreateRelease(context.Background(), ReleaseInput{
		GUID:        guid,
		Name:        name,
		SearchName:  searchName,
		CategoryID:  &c,
		ReleaseHash: guid, // unique per release
	})
	if err != nil {
		t.Fatalf("create release %q: %v", name, err)
	}
}

func TestSearchReleasesTokenizedAND(t *testing.T) {
	st := freshStore(t)
	ctx := context.Background()

	mkRelease(t, st, "g-sg", "Saving.Grace.S03E10.HDTV.XviD", "saving grace s03e10 hdtv xvid", 5030)
	mkRelease(t, st, "g-bn", "Burn.Notice.S02E14.720p.BluRay.x264", "burn notice s02e14 720p bluray x264", 5040)
	mkRelease(t, st, "g-mv", "Some.Movie.2024.1080p.BluRay.x264", "some movie 2024 1080p bluray x264", 2040)

	cases := []struct {
		name  string
		query string
		want  []string // expected GUIDs
	}{
		{"single word", "grace", []string{"g-sg"}},
		{"tvsearch series+ep non-adjacent", "saving s03e10", []string{"g-sg"}},
		{"multi-word any order", "xvid saving", []string{"g-sg"}},
		{"series only matches its episode", "burn s02e14", []string{"g-bn"}},
		{"no match when a token is absent", "saving s99e99", nil},
		{"empty query matches all", "", []string{"g-sg", "g-bn", "g-mv"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rels, total, err := st.SearchReleases(ctx, SearchFilter{Query: tc.query, Limit: 100})
			if err != nil {
				t.Fatal(err)
			}
			if total != len(tc.want) {
				t.Errorf("total = %d, want %d", total, len(tc.want))
			}
			got := map[string]bool{}
			for _, r := range rels {
				got[r.GUID] = true
			}
			for _, w := range tc.want {
				if !got[w] {
					t.Errorf("expected GUID %q in results for query %q", w, tc.query)
				}
			}
			if len(rels) != len(tc.want) {
				t.Errorf("result count = %d, want %d", len(rels), len(tc.want))
			}
		})
	}
}
