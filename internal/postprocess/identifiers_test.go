package postprocess

import (
	"strings"
	"testing"

	"github.com/vindicare/goindex/internal/store"
)

func has(ids []store.ReleaseIdentifier, source, id string) bool {
	for _, x := range ids {
		if x.Source == source && x.Identifier == id {
			return true
		}
	}
	return false
}

// TestExtractIdentifiersFromNFO is the falsifying test for #194: before the
// extractor existed, release_identifiers was empty for every release and every
// id-based search returned nothing.
func TestExtractIdentifiersFromNFO(t *testing.T) {
	nfo := `
    .:: The Shawshank Redemption (1994) ::.

    Video ..... 1920x1080 @ 8000 kbps
    Audio ..... DTS 1509 kbps
    Runtime ... 142 min
    IMDb ...... http://www.imdb.com/title/tt0111161/
    TMDb ...... https://www.themoviedb.org/movie/278
    Source .... BluRay 2009-03-15
`
	ids := ExtractIdentifiers(nfo)
	if !has(ids, store.IDSourceIMDB, "tt0111161") {
		t.Errorf("IMDb id not extracted: %+v", ids)
	}
	if !has(ids, store.IDSourceTMDB, "278") {
		t.Errorf("TMDB id not extracted: %+v", ids)
	}
	// Bitrates, resolutions, runtimes and dates are bare integers and must not
	// become identifiers.
	for _, x := range ids {
		for _, junk := range []string{"8000", "1509", "142", "1920", "1080", "2009"} {
			if x.Identifier == junk {
				t.Errorf("noise %q extracted as %s id", junk, x.Source)
			}
		}
	}
}

func TestExtractIdentifiersTVDB(t *testing.T) {
	ids := ExtractIdentifiers(`Series info: https://thetvdb.com/series/121361 and id=999`)
	if !has(ids, store.IDSourceTVDB, "121361") {
		t.Errorf("TVDB id not extracted: %+v", ids)
	}
}

// A TVDB/TMDB id is only safe to take from a URL that names the site; a bare
// integer anywhere in an NFO must never become an identifier.
func TestExtractIdentifiersIgnoresBareIntegers(t *testing.T) {
	if ids := ExtractIdentifiers("Encoded at 2000 kbps, 5000 frames, id 12345"); len(ids) != 0 {
		t.Errorf("bare integers extracted as identifiers: %+v", ids)
	}
}

func TestExtractIdentifiersEmptyAndDeduped(t *testing.T) {
	if ids := ExtractIdentifiers("   "); ids != nil {
		t.Errorf("blank NFO yielded %+v", ids)
	}
	ids := ExtractIdentifiers("tt0111161 tt0111161 http://imdb.com/title/tt0111161/")
	if len(ids) != 1 {
		t.Errorf("duplicate ids not collapsed: %+v", ids)
	}
}

// A pathological NFO must not pollute id search with a long list of matches.
func TestExtractIdentifiersBounded(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 50; i++ {
		b.WriteString("tt")
		b.WriteString(strings.Repeat("0", 6))
		b.WriteByte(byte('0' + i%10))
		b.WriteByte(' ')
	}
	if ids := ExtractIdentifiers(b.String()); len(ids) > maxNFOIdentifiers {
		t.Errorf("extracted %d identifiers, cap is %d", len(ids), maxNFOIdentifiers)
	}
}

// An id-shaped token outside a site URL must not be picked up: the URL prefix
// is the only thing distinguishing a TVDB id from any other integer.
func TestExtractIdentifiersTVDBRequiresURLContext(t *testing.T) {
	for _, nfo := range []string{
		"series id=121361",
		"see thetvdb.com for details, id=121361",
	} {
		if ids := ExtractIdentifiers(nfo); len(ids) != 0 {
			t.Errorf("extracted %+v from %q, expected none", ids, nfo)
		}
	}
	// The query-string form real NFOs use must still work.
	ids := ExtractIdentifiers("https://thetvdb.com/?tab=series&id=121361")
	if !has(ids, store.IDSourceTVDB, "121361") {
		t.Errorf("query-string TVDB id not extracted: %+v", ids)
	}
}
