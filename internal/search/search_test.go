package search

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vindicare/goindex/internal/store"
)

// fakeOS is a minimal OpenSearch stand-in: it records indexed/deleted docs and
// answers _search from them, so backend behaviour can be tested without a real
// OpenSearch instance.
type fakeOS struct {
	docs map[string]releaseDoc
}

func newFakeOS() *httptest.Server {
	f := &fakeOS{docs: map[string]releaseDoc{}}
	mux := http.NewServeMux()
	// PUT/DELETE /{index}/_doc/{guid}
	mux.HandleFunc("/goindex-releases/_doc/", func(w http.ResponseWriter, r *http.Request) {
		guid := strings.TrimPrefix(r.URL.Path, "/goindex-releases/_doc/")
		switch r.Method {
		case http.MethodPut:
			var d releaseDoc
			_ = json.NewDecoder(r.Body).Decode(&d)
			f.docs[guid] = d
			w.WriteHeader(http.StatusOK)
		case http.MethodDelete:
			if _, ok := f.docs[guid]; !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			delete(f.docs, guid)
			w.WriteHeader(http.StatusOK)
		}
	})
	// POST /{index}/_search
	mux.HandleFunc("/goindex-releases/_search", func(w http.ResponseWriter, r *http.Request) {
		var resp osSearchResponse
		resp.Hits.Total.Value = len(f.docs)
		for _, d := range f.docs {
			hit := struct {
				Source releaseDoc `json:"_source"`
			}{Source: d}
			resp.Hits.Hits = append(resp.Hits.Hits, hit)
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
	return httptest.NewServer(mux)
}

func rel(guid, name string) store.Release {
	posted := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	return store.Release{GUID: guid, Name: name, SearchName: strings.ToLower(name), SizeBytes: 100, PostedAt: &posted, CreatedAt: posted}
}

func TestOpenSearchIndexSearchDelete(t *testing.T) {
	srv := newFakeOS()
	defer srv.Close()
	os := NewOpenSearchBackend(srv.URL, "goindex-releases", time.Second)
	ctx := context.Background()

	if err := os.IndexRelease(ctx, rel("g1", "Ubuntu ISO")); err != nil {
		t.Fatalf("index: %v", err)
	}
	if err := os.IndexRelease(ctx, rel("g2", "Debian ISO")); err != nil {
		t.Fatalf("index: %v", err)
	}

	res, err := os.Search(ctx, store.SearchFilter{Query: "iso", Limit: 50})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if res.Total != 2 || len(res.Releases) != 2 {
		t.Fatalf("want 2 hits, got total=%d len=%d", res.Total, len(res.Releases))
	}

	if err := os.DeleteRelease(ctx, "g1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	// Deleting a missing doc is idempotent (404 tolerated).
	if err := os.DeleteRelease(ctx, "missing"); err != nil {
		t.Fatalf("idempotent delete: %v", err)
	}
	res, err = os.Search(ctx, store.SearchFilter{Limit: 50})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if res.Total != 1 {
		t.Fatalf("want 1 after delete, got %d", res.Total)
	}
}

func TestOpenSearchSearchErrorSurfaces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	os := NewOpenSearchBackend(srv.URL, "goindex-releases", time.Second)
	if _, err := os.Search(context.Background(), store.SearchFilter{Limit: 10}); err == nil {
		t.Fatal("expected error from failing OpenSearch")
	}
}

// stubBackend is a Backend + Indexer for exercising FallbackBackend.
type stubBackend struct {
	name    string
	err     error
	result  store.SearchResult
	indexed []string
	deleted []string
}

func (s *stubBackend) Name() string { return s.name }
func (s *stubBackend) Search(context.Context, store.SearchFilter) (store.SearchResult, error) {
	return s.result, s.err
}
func (s *stubBackend) IndexRelease(_ context.Context, r store.Release) error {
	s.indexed = append(s.indexed, r.GUID)
	return nil
}
func (s *stubBackend) DeleteRelease(_ context.Context, guid string) error {
	s.deleted = append(s.deleted, guid)
	return nil
}

func TestFallbackUsesPrimaryThenFallsBack(t *testing.T) {
	primaryOK := &stubBackend{name: "opensearch", result: store.SearchResult{Total: 5}}
	secondary := &stubBackend{name: "postgres", result: store.SearchResult{Total: 9}}
	fb := NewFallback(primaryOK, secondary, nil)

	res, err := fb.Search(context.Background(), store.SearchFilter{})
	if err != nil || res.Total != 5 {
		t.Fatalf("expected primary result 5, got total=%d err=%v", res.Total, err)
	}

	var fellBack bool
	primaryErr := &stubBackend{name: "opensearch", err: errors.New("down")}
	fb2 := NewFallback(primaryErr, secondary, func(error) { fellBack = true })
	res, err = fb2.Search(context.Background(), store.SearchFilter{})
	if err != nil || res.Total != 9 {
		t.Fatalf("expected fallback result 9, got total=%d err=%v", res.Total, err)
	}
	if !fellBack {
		t.Fatal("onFallback should have been called")
	}
}

func TestFallbackForwardsIndexingToPrimary(t *testing.T) {
	primary := &stubBackend{name: "opensearch"}
	secondary := &stubBackend{name: "postgres"}
	fb := NewFallback(primary, secondary, nil)
	if err := fb.IndexRelease(context.Background(), store.Release{GUID: "g1"}); err != nil {
		t.Fatal(err)
	}
	if err := fb.DeleteRelease(context.Background(), "g2"); err != nil {
		t.Fatal(err)
	}
	if len(primary.indexed) != 1 || primary.indexed[0] != "g1" {
		t.Fatalf("index not forwarded to primary: %v", primary.indexed)
	}
	if len(primary.deleted) != 1 || primary.deleted[0] != "g2" {
		t.Fatalf("delete not forwarded to primary: %v", primary.deleted)
	}
	if len(secondary.indexed) != 0 || len(secondary.deleted) != 0 {
		t.Fatal("secondary should not be indexed (authoritative)")
	}
}

func TestPostgresBackendDelegatesAndNoOpsIndexing(t *testing.T) {
	called := false
	pg := NewPostgresBackend(func(_ context.Context, f store.SearchFilter) (store.SearchResult, error) {
		called = true
		return store.SearchResult{Total: 7}, nil
	})
	res, err := pg.Search(context.Background(), store.SearchFilter{})
	if err != nil || !called || res.Total != 7 {
		t.Fatalf("delegate failed: called=%v total=%d err=%v", called, res.Total, err)
	}
	if pg.Name() != "postgres" {
		t.Fatalf("name=%q", pg.Name())
	}
	// Indexing is a no-op for the authoritative backend.
	if err := pg.IndexRelease(context.Background(), store.Release{GUID: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := pg.DeleteRelease(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
}

func TestReindexPagesAndIndexesAll(t *testing.T) {
	// Simulate a two-page store: first call returns a full page with a cursor,
	// second returns a partial page ending the scan.
	page := 0
	c := &store.SearchCursor{ID: 1, Sort: time.Now()}
	search := func(_ context.Context, f store.SearchFilter) (store.SearchResult, error) {
		if !f.IncludeObfuscated {
			t.Fatal("reindex must include obfuscated releases")
		}
		page++
		switch page {
		case 1:
			return store.SearchResult{
				Releases:   []store.Release{rel("g1", "A"), rel("g2", "B")},
				HasMore:    true,
				NextCursor: c,
			}, nil
		default:
			return store.SearchResult{Releases: []store.Release{rel("g3", "C")}}, nil
		}
	}
	ix := &stubBackend{name: "opensearch"}
	n, err := Reindex(context.Background(), ix, search, 2)
	if err != nil {
		t.Fatalf("reindex: %v", err)
	}
	if n != 3 || len(ix.indexed) != 3 {
		t.Fatalf("want 3 indexed, got n=%d indexed=%v", n, ix.indexed)
	}
}
