package newznab

import (
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vindicare/goindex/internal/store"
)

// This file hardens Newznab client compatibility (#136) with contract tests
// that parse the actual XML and assert the exact shapes Prowlarr, Sonarr, and
// Radarr depend on: caps, search/tvsearch/movie, details, get, categories +
// parents, pagination, empty results, malformed requests, RSS field fidelity,
// error codes, and content types. Rate-limit / API-key contract (Retry-After,
// X-RateLimit-*, 401/429) is covered end-to-end in
// internal/auth (RequireAPIKey middleware) which wraps this handler.

func contractRepo() *mockRepo {
	posted := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	return &mockRepo{
		cats: []store.Category{
			{ID: 2000, Name: "Movies"},
			{ID: 2040, ParentID: intp(2000), Name: "Movies/HD"},
			{ID: 2030, ParentID: intp(2000), Name: "Movies/SD"},
			{ID: 5000, Name: "TV"},
			{ID: 5040, ParentID: intp(5000), Name: "TV/HD"},
		},
		releases: []store.Release{
			{
				ID: 1, GUID: "abc-123", Name: "Some.Movie.2024.1080p.BluRay.x264",
				CategoryID: intp(2040), SizeBytes: 1500000000, Grabs: 5, PostedAt: &posted,
			},
		},
		total: 1,
	}
}

func doNZB(t *testing.T, h *Handler, url string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))
	return rec
}

// --- caps ---

func TestContractCapsShape(t *testing.T) {
	h := newTestHandler(contractRepo(), &mockNZB{})
	rec := doNZB(t, h, "/api?t=caps")
	if rec.Code != http.StatusOK {
		t.Fatalf("caps status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "xml") {
		t.Errorf("caps content-type = %q, want xml", ct)
	}
	var c caps
	if err := xml.Unmarshal(rec.Body.Bytes(), &c); err != nil {
		t.Fatalf("caps not valid XML: %v", err)
	}
	if c.Server.Title == "" || c.Server.Version == "" {
		t.Error("caps server must advertise title and version")
	}
	if c.Limits.Max != 100 || c.Limits.Default != 50 {
		t.Errorf("caps limits = max %d default %d, want 100/50", c.Limits.Max, c.Limits.Default)
	}
	// Search types must be available with q + cat at minimum.
	for name, s := range map[string]capsSearch{
		"search": c.Searching.Search, "tv-search": c.Searching.TVSearch,
		"movie-search": c.Searching.MovieSearch,
	} {
		if s.Available != "yes" {
			t.Errorf("%s should be available", name)
		}
		if !strings.Contains(s.SupportedParams, "q") || !strings.Contains(s.SupportedParams, "cat") {
			t.Errorf("%s supportedParams %q must include q and cat", name, s.SupportedParams)
		}
	}
	// Every advertised search param must be one the handler actually resolves.
	assertNoUnsupportedParams(t, c.Searching.TVSearch.SupportedParams)
	assertNoUnsupportedParams(t, c.Searching.MovieSearch.SupportedParams)

	// Categories nest parents with subcats.
	var movies *capsCategory
	for i := range c.Categories.Category {
		if c.Categories.Category[i].ID == 2000 {
			movies = &c.Categories.Category[i]
		}
	}
	if movies == nil {
		t.Fatal("caps missing parent category 2000 (Movies)")
	}
	if len(movies.Subcat) != 2 {
		t.Errorf("Movies should have 2 subcats (HD, SD), got %d", len(movies.Subcat))
	}
	// Subcats must not appear as top-level categories.
	for _, cat := range c.Categories.Category {
		if cat.ID == 2040 || cat.ID == 2030 {
			t.Errorf("subcategory %d must not be a top-level category", cat.ID)
		}
	}
}

// assertNoUnsupportedParams checks the advertised params are all ones the
// handler resolves (q, cat, season, ep, imdbid, tvdbid, tmdbid, limit, offset).
func assertNoUnsupportedParams(t *testing.T, params string) {
	t.Helper()
	supported := map[string]bool{
		"q": true, "cat": true, "season": true, "ep": true,
		"imdbid": true, "tvdbid": true, "tmdbid": true, "limit": true, "offset": true,
	}
	for _, p := range strings.Split(params, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if !supported[p] {
			t.Errorf("caps advertises unsupported param %q", p)
		}
	}
}

// --- search RSS field fidelity ---

func TestContractSearchRSSFields(t *testing.T) {
	h := newTestHandler(contractRepo(), &mockNZB{})
	rec := doNZB(t, h, "/api?t=search&q=movie")
	if rec.Code != http.StatusOK {
		t.Fatalf("search status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "xml") {
		t.Errorf("search content-type = %q, want xml", ct)
	}
	body := rec.Body.String()
	// Namespaced elements/attrs (newznab:) don't round-trip symmetrically through
	// Go's encoding/xml, so assert them on the rendered body — which is exactly
	// what clients consume.
	if !strings.Contains(body, `xmlns:newznab="`+newznabNS+`"`) {
		t.Errorf("feed missing newznab namespace declaration:\n%s", body)
	}
	if !strings.Contains(body, `<newznab:response offset="0" total="1">`) {
		t.Errorf("feed missing/incorrect newznab:response:\n%s", body)
	}
	if !strings.Contains(body, `<newznab:attr name="size" value="1500000000">`) {
		t.Errorf("feed missing size attr:\n%s", body)
	}
	if !strings.Contains(body, `<newznab:attr name="grabs" value="5">`) {
		t.Errorf("feed missing grabs attr:\n%s", body)
	}
	if !strings.Contains(body, `<newznab:attr name="category" value="2040">`) {
		t.Errorf("feed missing category attr:\n%s", body)
	}

	var feed rss
	if err := xml.Unmarshal(rec.Body.Bytes(), &feed); err != nil {
		t.Fatalf("search feed not valid XML: %v", err)
	}
	if feed.Version != "2.0" {
		t.Errorf("rss version = %q, want 2.0", feed.Version)
	}
	if len(feed.Channel.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(feed.Channel.Items))
	}
	it := feed.Channel.Items[0]
	if it.Title != "Some.Movie.2024.1080p.BluRay.x264" {
		t.Errorf("item title = %q", it.Title)
	}
	if it.GUID != "abc-123" {
		t.Errorf("item guid = %q, want abc-123", it.GUID)
	}
	// pubDate must be RFC1123Z (what clients parse).
	if _, err := time.Parse(time.RFC1123Z, it.PubDate); err != nil {
		t.Errorf("pubDate %q not RFC1123Z: %v", it.PubDate, err)
	}
	// Enclosure: absolute URL, correct length + NZB type.
	if !strings.HasPrefix(it.Enclosure.URL, "http://idx.local:8080/api?t=get&id=abc-123") {
		t.Errorf("enclosure url = %q, want absolute get URL", it.Enclosure.URL)
	}
	if it.Enclosure.Length != 1500000000 {
		t.Errorf("enclosure length = %d, want 1500000000", it.Enclosure.Length)
	}
	if it.Enclosure.Type != "application/x-nzb" {
		t.Errorf("enclosure type = %q, want application/x-nzb", it.Enclosure.Type)
	}
	// The link and enclosure URL should match (clients use either).
	if it.Link != it.Enclosure.URL {
		t.Errorf("item link %q != enclosure url %q", it.Link, it.Enclosure.URL)
	}
}

// --- pagination ---

func TestContractPaginationPassthrough(t *testing.T) {
	repo := contractRepo()
	repo.total = 250
	h := newTestHandler(repo, &mockNZB{})
	rec := doNZB(t, h, "/api?t=search&q=x&limit=25&offset=50")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if repo.lastFilt.Limit != 25 {
		t.Errorf("filter limit = %d, want 25", repo.lastFilt.Limit)
	}
	if repo.lastFilt.Offset != 50 {
		t.Errorf("filter offset = %d, want 50", repo.lastFilt.Offset)
	}
	// The namespaced response echoes offset + total for client paging.
	if !strings.Contains(rec.Body.String(), `<newznab:response offset="50" total="250">`) {
		t.Errorf("response should echo offset=50 total=250:\n%s", rec.Body.String())
	}
}

func TestContractLimitClampedToMax(t *testing.T) {
	repo := contractRepo()
	h := newTestHandler(repo, &mockNZB{})
	doNZB(t, h, "/api?t=search&q=x&limit=99999")
	if repo.lastFilt.Limit != 100 {
		t.Errorf("limit clamped to %d, want 100 (max)", repo.lastFilt.Limit)
	}
}

// --- category expansion ---

func TestContractCategoryFilterExpansion(t *testing.T) {
	repo := contractRepo()
	h := newTestHandler(repo, &mockNZB{})
	// A parent category (2000) plus explicit subcat (5040) and multiple values.
	doNZB(t, h, "/api?t=search&q=x&cat=2000,5040")
	if len(repo.lastFilt.Categories) != 2 {
		t.Fatalf("categories = %v, want [2000 5040]", repo.lastFilt.Categories)
	}
}

// --- empty results ---

func TestContractEmptyResults(t *testing.T) {
	repo := &mockRepo{releases: nil, total: 0}
	h := newTestHandler(repo, &mockNZB{})
	rec := doNZB(t, h, "/api?t=search&q=nothingmatches")
	if rec.Code != http.StatusOK {
		t.Fatalf("empty search status = %d, want 200", rec.Code)
	}
	var feed rss
	if err := xml.Unmarshal(rec.Body.Bytes(), &feed); err != nil {
		t.Fatalf("empty feed not valid XML: %v", err)
	}
	if len(feed.Channel.Items) != 0 {
		t.Errorf("empty feed should have no items, got %d", len(feed.Channel.Items))
	}
	if !strings.Contains(rec.Body.String(), `total="0"`) {
		t.Errorf("empty feed should report total=0:\n%s", rec.Body.String())
	}
}

// --- details ---

func TestContractDetails(t *testing.T) {
	h := newTestHandler(contractRepo(), &mockNZB{})
	rec := doNZB(t, h, "/api?t=details&id=abc-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("details status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `total="1"`) {
		t.Errorf("details should report total=1:\n%s", rec.Body.String())
	}
	var feed rss
	if err := xml.Unmarshal(rec.Body.Bytes(), &feed); err != nil {
		t.Fatalf("details not valid XML: %v", err)
	}
	if len(feed.Channel.Items) != 1 {
		t.Errorf("details should return exactly one item, got %d", len(feed.Channel.Items))
	}
	if len(feed.Channel.Items) == 1 && feed.Channel.Items[0].GUID != "abc-123" {
		t.Errorf("details guid = %q", feed.Channel.Items[0].GUID)
	}
}

func TestContractDetailsUnknownItem(t *testing.T) {
	h := newTestHandler(contractRepo(), &mockNZB{})
	rec := doNZB(t, h, "/api?t=details&id=does-not-exist")
	assertNZBError(t, rec, 300)
}

// --- get / NZB download ---

func TestContractGetContentType(t *testing.T) {
	repo := contractRepo()
	nzb := &mockNZB{data: []byte("<nzb>...</nzb>"), filename: "Some.Movie.2024.nzb"}
	h := newTestHandler(repo, nzb)
	rec := doNZB(t, h, "/api?t=get&id=abc-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/x-nzb" {
		t.Errorf("get content-type = %q, want application/x-nzb", ct)
	}
	cd := rec.Header().Get("Content-Disposition")
	if !strings.Contains(cd, "attachment") || !strings.Contains(cd, "Some.Movie.2024.nzb") {
		t.Errorf("Content-Disposition = %q, want attachment with filename", cd)
	}
	if rec.Body.String() != "<nzb>...</nzb>" {
		t.Errorf("get body = %q", rec.Body.String())
	}
	// A successful download increments the grab counter.
	if repo.grabs != 1 {
		t.Errorf("grabs incremented %d times, want 1", repo.grabs)
	}
}

func TestContractGetUnknownItem(t *testing.T) {
	nzb := &mockNZB{err: store.ErrNotFound}
	h := newTestHandler(contractRepo(), nzb)
	rec := doNZB(t, h, "/api?t=get&id=missing")
	assertNZBError(t, rec, 300)
}

// --- malformed / error codes ---

func TestContractErrorCodes(t *testing.T) {
	h := newTestHandler(contractRepo(), &mockNZB{})

	// Missing get id -> code 200 (missing parameter).
	assertNZBError(t, doNZB(t, h, "/api?t=get"), 200)
	// Unknown function -> code 202.
	assertNZBError(t, doNZB(t, h, "/api?t=bogus"), 202)
}

// assertNZBError checks the response is a well-formed newznab <error> with the
// given code (Newznab returns HTTP 200 with an error body).
func assertNZBError(t *testing.T, rec *httptest.ResponseRecorder, wantCode int) {
	t.Helper()
	var e nnError
	if err := xml.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("error response not valid XML: %v\nbody: %s", err, rec.Body.String())
	}
	if e.Code != wantCode {
		t.Errorf("error code = %d, want %d (body: %s)", e.Code, wantCode, rec.Body.String())
	}
	if e.Description == "" {
		t.Error("error must carry a description")
	}
}

// --- client workflow sequences ---

// TestContractProwlarrCapsThenSearch mimics Prowlarr's flow: fetch caps, then a
// tvsearch, then a movie search, then details, then get.
func TestContractClientWorkflow(t *testing.T) {
	repo := contractRepo()
	nzb := &mockNZB{data: []byte("<nzb/>"), filename: "r.nzb"}
	h := newTestHandler(repo, nzb)

	steps := []struct {
		name string
		url  string
	}{
		{"caps", "/api?t=caps"},
		{"tvsearch", "/api?t=tvsearch&q=show&season=3&ep=10&cat=5000"},
		{"movie", "/api?t=movie&q=film&cat=2000"},
		{"search", "/api?t=search&q=anything"},
		{"details", "/api?t=details&id=abc-123"},
		{"get", "/api?t=get&id=abc-123"},
	}
	for _, s := range steps {
		rec := doNZB(t, h, s.url)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status = %d, want 200", s.name, rec.Code)
		}
	}
	// tvsearch season/ep folded into the query.
	// (movie was the last search before details/get; verify tvsearch produced
	// the SxxEyy token by re-running it and inspecting the filter.)
	doNZB(t, h, "/api?t=tvsearch&q=show&season=3&ep=10")
	if !strings.Contains(repo.lastFilt.Query, "s03e10") {
		t.Errorf("tvsearch query = %q, want it to contain s03e10", repo.lastFilt.Query)
	}
}
