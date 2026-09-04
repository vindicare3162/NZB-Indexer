package store

import (
	"context"
	"testing"
)

func TestNormalizeIdentifier(t *testing.T) {
	cases := []struct {
		name       string
		source     string
		in         string
		wantSource string
		wantID     string
		wantOK     bool
	}{
		{"imdb prefixed", "imdb", "tt0111161", "imdb", "tt0111161", true},
		{"imdb bare 7", "imdb", "0111161", "imdb", "tt0111161", true},
		{"imdb bare short padded", "imdb", "12345", "imdb", "tt0012345", true},
		{"imdb uppercase source", "IMDB", "tt0111161", "imdb", "tt0111161", true},
		{"imdb whitespace", "imdb", "  tt0111161 ", "imdb", "tt0111161", true},
		{"imdb non-numeric", "imdb", "ttabcdef", "", "", false},
		{"imdb empty", "imdb", "", "", "", false},
		{"tvdb digits", "tvdb", "81189", "tvdb", "81189", true},
		{"tmdb digits", "tmdb", "1396", "tmdb", "1396", true},
		{"tvdb non-numeric", "tvdb", "tt123", "", "", false},
		{"unknown source", "rid", "1234", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := NormalizeIdentifier(tc.source, tc.in)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (got %+v)", ok, tc.wantOK, got)
			}
			if !ok {
				return
			}
			if got.Source != tc.wantSource || got.Identifier != tc.wantID {
				t.Errorf("got {%s %s}, want {%s %s}", got.Source, got.Identifier, tc.wantSource, tc.wantID)
			}
		})
	}
}

func releaseID(t *testing.T, st *Store, guid string) int64 {
	t.Helper()
	rel, err := st.GetReleaseByGUID(context.Background(), guid)
	if err != nil {
		t.Fatalf("get release %q: %v", guid, err)
	}
	return rel.ID
}

func TestAddAndGetReleaseIdentifiers(t *testing.T) {
	st := freshStore(t)
	ctx := context.Background()

	mkRelease(t, st, "rel-ids-1", "Some.Movie.2024.1080p", "some movie 2024 1080p", 2040)
	id := releaseID(t, st, "rel-ids-1")

	// Add an imdb (bare, gets normalized) + tmdb id.
	if err := st.AddReleaseIdentifier(ctx, id, "imdb", "0111161"); err != nil {
		t.Fatalf("add imdb: %v", err)
	}
	if err := st.AddReleaseIdentifier(ctx, id, "tmdb", "1396"); err != nil {
		t.Fatalf("add tmdb: %v", err)
	}

	// Idempotent: re-adding the same (even in a different input form) is a no-op.
	if err := st.AddReleaseIdentifier(ctx, id, "imdb", "tt0111161"); err != nil {
		t.Fatalf("re-add imdb: %v", err)
	}

	ids, err := st.GetReleaseIdentifiers(ctx, id)
	if err != nil {
		t.Fatalf("get identifiers: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("expected 2 identifiers after idempotent re-add, got %d: %+v", len(ids), ids)
	}
	// Ordered by source: imdb before tmdb.
	if ids[0] != (ReleaseIdentifier{Source: "imdb", Identifier: "tt0111161"}) {
		t.Errorf("ids[0] = %+v", ids[0])
	}
	if ids[1] != (ReleaseIdentifier{Source: "tmdb", Identifier: "1396"}) {
		t.Errorf("ids[1] = %+v", ids[1])
	}

	// Invalid identifier is rejected and stores nothing.
	if err := st.AddReleaseIdentifier(ctx, id, "imdb", "not-a-number"); err == nil {
		t.Error("expected error adding invalid imdb identifier")
	}
}

func TestSearchReleasesByIdentifier(t *testing.T) {
	st := freshStore(t)
	ctx := context.Background()

	mkRelease(t, st, "match-1", "The.Matched.Movie.2024", "the matched movie 2024", 2040)
	mkRelease(t, st, "other-1", "Unrelated.Movie.2024", "unrelated movie 2024", 2040)

	matchID := releaseID(t, st, "match-1")
	if err := st.AddReleaseIdentifier(ctx, matchID, "imdb", "tt0111161"); err != nil {
		t.Fatalf("add imdb: %v", err)
	}
	if err := st.AddReleaseIdentifier(ctx, matchID, "tmdb", "1396"); err != nil {
		t.Fatalf("add tmdb: %v", err)
	}

	// Single-identifier filter returns only the release carrying it.
	rels, total, err := st.SearchReleases(ctx, SearchFilter{
		Limit:       100,
		Identifiers: []ReleaseIdentifier{{Source: "imdb", Identifier: "tt0111161"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || len(rels) != 1 || rels[0].GUID != "match-1" {
		t.Fatalf("imdb filter: total=%d rels=%d, want the one matching release", total, len(rels))
	}

	// Multiple identifiers are ANDed: the release must carry all of them.
	rels, total, err = st.SearchReleases(ctx, SearchFilter{
		Limit: 100,
		Identifiers: []ReleaseIdentifier{
			{Source: "imdb", Identifier: "tt0111161"},
			{Source: "tmdb", Identifier: "1396"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || len(rels) != 1 || rels[0].GUID != "match-1" {
		t.Fatalf("imdb+tmdb filter: total=%d rels=%d, want the release carrying both", total, len(rels))
	}

	// A filter with an unmatched identifier returns nothing.
	rels, total, err = st.SearchReleases(ctx, SearchFilter{
		Limit: 100,
		Identifiers: []ReleaseIdentifier{
			{Source: "imdb", Identifier: "tt0111161"},
			{Source: "tvdb", Identifier: "99999"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if total != 0 || len(rels) != 0 {
		t.Fatalf("partial-match filter should return nothing; got total=%d rels=%d", total, len(rels))
	}
}
