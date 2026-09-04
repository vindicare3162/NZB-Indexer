package newznab

import (
	"context"
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vindicare/goindex/internal/store"
)

// mockRepo implements Repo for handler tests.
type mockRepo struct {
	cats     []store.Category
	releases []store.Release
	total    int
	lastFilt store.SearchFilter
	grabs    int
}

func (m *mockRepo) ListCategories(context.Context) ([]store.Category, error) {
	return m.cats, nil
}

func (m *mockRepo) SearchReleases(_ context.Context, f store.SearchFilter) ([]store.Release, int, error) {
	m.lastFilt = f
	return m.releases, m.total, nil
}

func (m *mockRepo) GetReleaseByGUID(_ context.Context, guid string) (store.Release, error) {
	for _, r := range m.releases {
		if r.GUID == guid {
			return r, nil
		}
	}
	return store.Release{}, store.ErrNotFound
}

func (m *mockRepo) IncrementGrabs(context.Context, int64) error {
	m.grabs++
	return nil
}

// mockNZB implements NZBGenerator.
type mockNZB struct {
	data     []byte
	filename string
	err      error
}

func (m *mockNZB) ForGUID(context.Context, string) ([]byte, string, error) {
	return m.data, m.filename, m.err
}

func intp(i int) *int { return &i }

func newTestHandler(repo Repo, nzb NZBGenerator) *Handler {
	return NewHandler(repo, nzb, Config{BaseURL: "http://idx.local:8080", MaxLimit: 100, DefaultLimit: 50})
}

func TestCaps(t *testing.T) {
	repo := &mockRepo{cats: []store.Category{
		{ID: 2000, Name: "Movies"},
		{ID: 2040, ParentID: intp(2000), Name: "Movies/HD"},
		{ID: 5000, Name: "TV"},
	}}
	h := newTestHandler(repo, &mockNZB{})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api?t=caps", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()

	// Parse it back and check structure.
	var c caps
	if err := xml.Unmarshal([]byte(body), &c); err != nil {
		t.Fatalf("parse caps: %v\n%s", err, body)
	}
	if c.Limits.Max != 100 || c.Limits.Default != 50 {
		t.Errorf("limits = %+v", c.Limits)
	}
	if c.Searching.TVSearch.Available != "yes" {
		t.Error("tv-search should be available")
	}
	// Advertised params match what the handler resolves: TV keeps season/ep and
	// now advertises the supported external ids (imdbid/tvdbid/tmdbid), which are
	// matched against stored normalized release identifiers (#108). Unsupported
	// legacy ids (rid) are still not advertised.
	tv := c.Searching.TVSearch.SupportedParams
	if !strings.Contains(tv, "season") || !strings.Contains(tv, "ep") {
		t.Errorf("tv-search params missing season/ep: %q", tv)
	}
	if !strings.Contains(tv, "tvdbid") || !strings.Contains(tv, "imdbid") {
		t.Errorf("tv-search should advertise supported ids: %q", tv)
	}
	if strings.Contains(tv, "rid") {
		t.Errorf("tv-search should not advertise unsupported id 'rid': %q", tv)
	}
	if !strings.Contains(c.Searching.MovieSearch.SupportedParams, "imdbid") {
		t.Errorf("movie-search should advertise imdbid: %q", c.Searching.MovieSearch.SupportedParams)
	}
	// Movies parent with an HD subcat.
	var moviesFound bool
	for _, cat := range c.Categories.Category {
		if cat.ID == 2000 {
			moviesFound = true
			if len(cat.Subcat) != 1 || cat.Subcat[0].ID != 2040 {
				t.Errorf("movies subcats = %+v", cat.Subcat)
			}
		}
	}
	if !moviesFound {
		t.Error("Movies category missing from caps")
	}
}

func TestSearchFeed(t *testing.T) {
	posted := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	repo := &mockRepo{
		total: 2,
		releases: []store.Release{
			{ID: 1, GUID: "guid-1", Name: "Show.S01E01.1080p", CategoryID: intp(5040), SizeBytes: 1500, PostedAt: &posted, Grabs: 3},
			{ID: 2, GUID: "guid-2", Name: "Movie.2024.1080p", CategoryID: intp(2040), SizeBytes: 900, PostedAt: &posted},
		},
	}
	h := newTestHandler(repo, &mockNZB{})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api?t=search&q=show&cat=5000&limit=10&offset=0", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()

	// The feed must declare the newznab namespace and carry newznab:attr.
	if !strings.Contains(body, `xmlns:newznab="`+newznabNS+`"`) {
		t.Errorf("missing newznab namespace declaration:\n%s", body)
	}
	if !strings.Contains(body, "<newznab:attr") {
		t.Errorf("missing newznab:attr elements:\n%s", body)
	}

	// Filter should have parsed the category and query.
	if repo.lastFilt.Query != "show" {
		t.Errorf("query = %q, want show", repo.lastFilt.Query)
	}
	if len(repo.lastFilt.Categories) != 1 || repo.lastFilt.Categories[0] != 5000 {
		t.Errorf("categories = %v, want [5000]", repo.lastFilt.Categories)
	}

	// The namespaced response element carries the total for pagination.
	// (Go's xml round-trip on prefixed elements is asymmetric, so assert on
	// the rendered output, which is what real clients consume.)
	if !strings.Contains(body, `<newznab:response offset="0" total="2">`) {
		t.Errorf("missing/incorrect newznab:response:\n%s", body)
	}

	// Parse response and check items.
	var parsed rss
	if err := xml.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("parse feed: %v", err)
	}
	if len(parsed.Channel.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(parsed.Channel.Items))
	}
	first := parsed.Channel.Items[0]
	if first.Title != "Show.S01E01.1080p" {
		t.Errorf("item title = %q", first.Title)
	}
	wantURL := "http://idx.local:8080/api?t=get&id=guid-1"
	if first.Enclosure.URL != wantURL {
		t.Errorf("enclosure url = %q, want %q", first.Enclosure.URL, wantURL)
	}
	if first.Enclosure.Length != 1500 {
		t.Errorf("enclosure length = %d, want 1500", first.Enclosure.Length)
	}
}

func TestTVSearchBuildsSeasonEpisodeQuery(t *testing.T) {
	repo := &mockRepo{}
	h := newTestHandler(repo, &mockNZB{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api?t=tvsearch&q=some+show&season=1&ep=5", nil))
	if rec.Code != http.StatusOK {
		t.Fatal(rec.Code)
	}
	if repo.lastFilt.Query != "some show s01e05" {
		t.Errorf("query = %q, want 'some show s01e05'", repo.lastFilt.Query)
	}
}

func TestGetServesNZB(t *testing.T) {
	repo := &mockRepo{releases: []store.Release{{ID: 1, GUID: "guid-1", Name: "Rel"}}}
	nzb := &mockNZB{data: []byte("<nzb>...</nzb>"), filename: "Rel.nzb"}
	h := newTestHandler(repo, nzb)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api?t=get&id=guid-1", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/x-nzb" {
		t.Errorf("content-type = %q", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "Rel.nzb") {
		t.Errorf("content-disposition = %q", cd)
	}
	if rec.Body.String() != "<nzb>...</nzb>" {
		t.Errorf("body = %q", rec.Body.String())
	}
	if repo.grabs != 1 {
		t.Errorf("grabs = %d, want 1 (should increment on download)", repo.grabs)
	}
}

func TestGetMissingID(t *testing.T) {
	h := newTestHandler(&mockRepo{}, &mockNZB{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api?t=get", nil))
	// Newznab errors are HTTP 200 with an <error> body.
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "<error") {
		t.Errorf("expected error body, got:\n%s", rec.Body.String())
	}
}

func TestUnknownFunction(t *testing.T) {
	h := newTestHandler(&mockRepo{}, &mockNZB{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api?t=bogus", nil))
	if !strings.Contains(rec.Body.String(), "No such function") {
		t.Errorf("expected 'No such function' error, got:\n%s", rec.Body.String())
	}
}

func TestMovieSearchIMDBIdentifier(t *testing.T) {
	repo := &mockRepo{releases: nil, total: 0}
	h := newTestHandler(repo, &mockNZB{})

	// q + imdbid: the id becomes a normalized identifier filter, NOT a text
	// token. The q text stays the query.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api?t=movie&q=Some.Movie.2024&imdbid=tt0111161", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if repo.lastFilt.Query != "Some.Movie.2024" {
		t.Errorf("query = %q, want just the q text (imdbid should not be a token)", repo.lastFilt.Query)
	}
	wantID := store.ReleaseIdentifier{Source: store.IDSourceIMDB, Identifier: "tt0111161"}
	if len(repo.lastFilt.Identifiers) != 1 || repo.lastFilt.Identifiers[0] != wantID {
		t.Errorf("identifiers = %+v, want [%+v]", repo.lastFilt.Identifiers, wantID)
	}

	// Bare imdbid (no tt prefix) normalizes to the same identifier and, with no
	// q text, drives the search purely by identifier (not an empty feed).
	repo.lastFilt = store.SearchFilter{}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api?t=movie&imdbid=0111161", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if repo.lastFilt.Query != "" {
		t.Errorf("bare imdbid query = %q, want empty", repo.lastFilt.Query)
	}
	if len(repo.lastFilt.Identifiers) != 1 || repo.lastFilt.Identifiers[0] != wantID {
		t.Errorf("bare imdbid identifiers = %+v, want [%+v]", repo.lastFilt.Identifiers, wantID)
	}
}

func TestIDOnlySearchReturnsEmptyNotEverything(t *testing.T) {
	// An id-only search with no resolvable token/identifier must NOT hit
	// SearchReleases (which would browse-all); it returns an empty feed. 'rid'
	// is a legacy id we don't map to a stored identifier, so it's unresolvable.
	repo := &mockRepo{releases: []store.Release{{GUID: "x", Name: "Should.Not.Appear"}}, total: 999}
	h := newTestHandler(repo, &mockNZB{})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api?t=tvsearch&rid=12345", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `total="0"`) {
		t.Errorf("expected empty feed (total=0), got: %s", body)
	}
	if strings.Contains(body, "Should.Not.Appear") {
		t.Error("id-only search returned catalogue items; must return nothing")
	}
	// SearchReleases should not have been called (Limit would be non-zero if it had).
	if repo.lastFilt.Limit != 0 || repo.lastFilt.Query != "" {
		t.Errorf("SearchReleases should not have been called for an unresolved id-only search, got filter %+v", repo.lastFilt)
	}
}

func TestResolvableIDOnlySearchFiltersByIdentifier(t *testing.T) {
	// A resolvable id-only search (e.g. tvdbid) DOES search, filtering by the
	// normalized identifier rather than returning an empty feed.
	repo := &mockRepo{releases: nil, total: 0}
	h := newTestHandler(repo, &mockNZB{})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api?t=tvsearch&tvdbid=81189", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	// SearchReleases should have run (Limit set) with the tvdb identifier filter.
	if repo.lastFilt.Limit == 0 {
		t.Error("resolvable id search should have called SearchReleases")
	}
	want := store.ReleaseIdentifier{Source: store.IDSourceTVDB, Identifier: "81189"}
	if len(repo.lastFilt.Identifiers) != 1 || repo.lastFilt.Identifiers[0] != want {
		t.Errorf("identifiers = %+v, want [%+v]", repo.lastFilt.Identifiers, want)
	}
}

func TestBareBrowseSearchStillReturnsResults(t *testing.T) {
	// A plain t=search with no q and no id is a browse feed: it should still
	// query (return recent releases), unlike an id-only search.
	repo := &mockRepo{releases: []store.Release{{GUID: "a", Name: "Recent.Release"}}, total: 1}
	h := newTestHandler(repo, &mockNZB{})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api?t=search", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Recent.Release") {
		t.Error("bare browse search should return recent releases")
	}
}
