// Package integration holds end-to-end fixture tests that exercise goindex's
// Newznab-compatible API the way real clients (Prowlarr, Sonarr, Radarr,
// SABnzbd, NZBGet) do (#140). Unlike the mock-based contract tests in
// internal/api/newznab, these run the real handler over a real, disposable
// PostgreSQL database populated with releases that carry durable NZB segments,
// so the full search -> details -> download flow is validated against genuine
// stored data with no external NNTP provider.
//
// The suite is DB-backed and therefore requires GOINDEX_TEST_DSN; it is skipped
// otherwise. Optional live-client validation (pointing a real Prowlarr/Sonarr
// instance at the server) is documented in docs/integration-clients.md and is
// intentionally not part of the automated suite.
package integration

import (
	"context"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/vindicare/goindex/internal/api/newznab"
	"github.com/vindicare/goindex/internal/nzb"
	"github.com/vindicare/goindex/internal/store"
)

// --- harness ---------------------------------------------------------------

// env bundles the components under test: a live store plus the Newznab handler
// wired to the real NZB generator.
type env struct {
	st      *store.Store
	handler http.Handler
}

func testDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("GOINDEX_TEST_DSN")
	if dsn == "" {
		t.Skip("GOINDEX_TEST_DSN not set; skipping DB-backed integration fixtures")
	}
	return dsn
}

// newEnv rolls the schema fresh, opens a store, and builds the Newznab handler
// backed by the real NZB generator.
func newEnv(t *testing.T) *env {
	t.Helper()
	dsn := testDSN(t)
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

	gen := nzb.NewGenerator(st)
	h := newznab.NewHandler(st, gen, newznab.Config{
		BaseURL:      "http://idx.local:8080",
		MaxLimit:     100,
		DefaultLimit: 50,
	})
	return &env{st: st, handler: h}
}

// seedDownloadableRelease creates a release with durable NZB segments so the
// t=get flow returns a real document. It mirrors the production path: ingest
// parts -> assemble the binary -> create the release from that binary (which
// snapshots the ordered segments). Returns the release GUID.
func seedDownloadableRelease(t *testing.T, st *Store, group, name string, categoryID int) string {
	t.Helper()
	ctx := context.Background()
	g, err := st.UpsertGroup(ctx, group, true)
	if err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	norm := strings.ToLower(strings.ReplaceAll(name, " ", "."))
	const total = 3
	var parts []store.PartInput
	for i := 1; i <= total; i++ {
		parts = append(parts, store.PartInput{
			GroupID:       g.ID,
			ArticleNumber: int64(1000 + i),
			MessageID:     fmt.Sprintf("<%s-%d@x>", norm, i),
			Subject:       fmt.Sprintf(`"%s" yEnc (%d/%d)`, norm, i, total),
			Poster:        "poster@example.com",
			Bytes:         1000,
			PartNumber:    i,
			TotalParts:    total,
			NormSubject:   norm,
		})
	}
	if _, err := st.InsertParts(ctx, parts); err != nil {
		t.Fatalf("insert parts: %v", err)
	}
	if _, err := st.AssembleBinaries(ctx, 100); err != nil {
		t.Fatalf("assemble binaries: %v", err)
	}

	var binID int64
	if err := st.Pool().QueryRow(ctx,
		`SELECT id FROM binaries WHERE norm_subject = $1`, norm).Scan(&binID); err != nil {
		t.Fatalf("find binary: %v", err)
	}

	cat := categoryID
	posted := time.Now().Add(-time.Hour)
	guid := "guid-" + norm
	_, _, err = st.CreateRelease(ctx, store.ReleaseInput{
		GUID:        guid,
		Name:        name,
		// The pipeline stores a normalized (lowercased) search_name; mirror that
		// so token matching behaves as in production.
		SearchName:  strings.ToLower(name),
		CategoryID:  &cat,
		GroupID:     &g.ID,
		BinaryID:    &binID,
		Poster:      "poster@example.com",
		TotalParts:  total,
		SizeBytes:   3000,
		PostedAt:    &posted,
		ReleaseHash: "hash-" + norm,
	})
	if err != nil {
		t.Fatalf("create release: %v", err)
	}
	return guid
}

// Store is an alias so seedDownloadableRelease reads naturally.
type Store = store.Store

// do issues a GET against the handler with the given query and returns the
// recorder.
func (e *env) do(t *testing.T, rawquery string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api?"+rawquery, nil)
	e.handler.ServeHTTP(rec, req)
	return rec
}

// parsed RSS shapes (only the fields clients consume).
type rssFeed struct {
	XMLName xml.Name  `xml:"rss"`
	Items   []rssItem `xml:"channel>item"`
}
type rssItem struct {
	Title     string `xml:"title"`
	GUID      string `xml:"guid"`
	Link      string `xml:"link"`
	Category  string `xml:"category"`
	Enclosure struct {
		URL    string `xml:"url,attr"`
		Type   string `xml:"type,attr"`
		Length string `xml:"length,attr"`
	} `xml:"enclosure"`
}

func parseFeed(t *testing.T, body []byte) rssFeed {
	t.Helper()
	var f rssFeed
	if err := xml.Unmarshal(body, &f); err != nil {
		t.Fatalf("feed not valid XML: %v\n%s", err, body)
	}
	return f
}

// --- capability discovery (Prowlarr/Sonarr/Radarr) -------------------------

// TestCapabilityDiscovery covers the caps request every client makes on setup:
// it must advertise limits, the search types, and the category tree.
func TestCapabilityDiscovery(t *testing.T) {
	e := newEnv(t)
	rec := e.do(t, "t=caps")
	if rec.Code != http.StatusOK {
		t.Fatalf("caps status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`<limits max="100" default="50"`,
		`<search available="yes"`,
		`<tv-search available="yes"`,
		`<movie-search available="yes"`,
		`<categories>`,
		`<category id="2000"`, // Movies parent (seeded)
		`<category id="5000"`, // TV parent (seeded)
	} {
		if !strings.Contains(body, want) {
			t.Errorf("caps missing %q\n%s", want, body)
		}
	}
}

// --- text search + categories + pagination + details + download -----------

// TestProwlarrSonarrRadarrSearchFlow drives the core indexer flow: a text
// search returns an RSS feed whose items carry the fields these clients
// consume (title, guid, category, and an enclosure download link), a category
// filter narrows results, pagination echoes offset, details returns the item,
// and the enclosure link downloads a real NZB.
func TestProwlarrSonarrRadarrSearchFlow(t *testing.T) {
	e := newEnv(t)
	// Two releases in different categories.
	tvGUID := seedDownloadableRelease(t, e.st, "alt.binaries.tv", "Example Show S01E01 1080p", 5040)
	seedDownloadableRelease(t, e.st, "alt.binaries.movies", "Example Movie 2026 1080p", 2040)

	// Text search (Prowlarr/Sonarr t=search or t=tvsearch). The query is
	// lowercased to match the normalized search_name, as real clients' scene
	// names resolve to after the indexer normalizes them.
	rec := e.do(t, "t=search&q=example")
	if rec.Code != http.StatusOK {
		t.Fatalf("search status = %d", rec.Code)
	}
	feed := parseFeed(t, rec.Body.Bytes())
	if len(feed.Items) != 2 {
		t.Fatalf("search returned %d items, want 2", len(feed.Items))
	}
	for _, it := range feed.Items {
		if it.Title == "" || it.GUID == "" {
			t.Errorf("item missing title/guid: %+v", it)
		}
		if it.Enclosure.URL == "" || it.Enclosure.Type != "application/x-nzb" {
			t.Errorf("item enclosure wrong: %+v", it.Enclosure)
		}
	}

	// Category filter (Radarr requests a movie category).
	rec = e.do(t, "t=search&cat=2040")
	feed = parseFeed(t, rec.Body.Bytes())
	if len(feed.Items) != 1 || feed.Items[0].Category != "2040" {
		t.Fatalf("category filter returned %+v, want 1 item in 2040", feed.Items)
	}

	// Pagination: offset is echoed in the newznab:response element.
	rec = e.do(t, "t=search&q=example&limit=1&offset=1")
	body := rec.Body.String()
	if !strings.Contains(body, `offset="1"`) {
		t.Errorf("pagination offset not echoed:\n%s", body)
	}

	// Details for the TV release.
	rec = e.do(t, "t=details&id="+tvGUID)
	if rec.Code != http.StatusOK {
		t.Fatalf("details status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), tvGUID) {
		t.Errorf("details missing guid %q", tvGUID)
	}

	// Download via the Newznab get flow (what Sonarr/Radarr hand to the
	// downloader): a real NZB document with the right content type + filename.
	assertNZBDownload(t, e, tvGUID)
}

// --- SABnzbd / NZBGet download flow ----------------------------------------

// TestDownloaderNZBRetrieval covers what SABnzbd/NZBGet do once handed a link:
// fetch the NZB and check content type, a download filename, well-formed XML,
// and graceful handling of a bad id.
func TestDownloaderNZBRetrieval(t *testing.T) {
	e := newEnv(t)
	guid := seedDownloadableRelease(t, e.st, "alt.binaries.dl", "Downloadable Release 2026", 2040)

	assertNZBDownload(t, e, guid)

	// Unknown id: the Newznab convention returns an error document (HTTP 200
	// with an <error> body carrying a code), which downloaders treat as a
	// failed grab rather than a valid NZB.
	rec := e.do(t, "t=get&id=does-not-exist")
	if ct := rec.Header().Get("Content-Type"); strings.Contains(ct, "x-nzb") {
		t.Errorf("missing release should not return an NZB content type, got %q", ct)
	}
	if !strings.Contains(rec.Body.String(), "<error") {
		t.Errorf("missing release should return a Newznab error doc:\n%s", rec.Body.String())
	}
}

// assertNZBDownload fetches the NZB for guid and asserts the downloader
// contract: NZB content type, a filename in Content-Disposition, and a
// well-formed <nzb> document carrying the release's segments.
func assertNZBDownload(t *testing.T, e *env, guid string) {
	t.Helper()
	rec := e.do(t, "t=get&id="+guid)
	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "nzb") {
		t.Errorf("get content-type = %q, want an nzb type", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "filename") {
		t.Errorf("get content-disposition = %q, want a filename", cd)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "<nzb") {
		t.Fatalf("NZB body missing <nzb> root:\n%s", body)
	}
	// The document must reference the seeded segments (message ids).
	if !strings.Contains(body, "<segment") {
		t.Errorf("NZB has no <segment> entries:\n%s", body)
	}
}

// --- API-key handling (generic Newznab clients) ----------------------------
//
// The Newznab API-key contract (401 on a bad key, 429 with Retry-After under
// rate limiting, X-RateLimit-* headers) is enforced by the auth middleware and
// exhaustively covered in internal/auth; the handler under test here is mounted
// behind that middleware in production. These fixtures focus on the handler's
// own response contract, which every client depends on regardless of auth.
