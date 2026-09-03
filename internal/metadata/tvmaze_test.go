package metadata

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTVMazeLookupMatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/singlesearch/shows" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": 82,
			"name": "Game of Thrones",
			"premiered": "2011-04-17",
			"summary": "<p>Seven noble families fight.</p>",
			"image": {"original": "https://example.com/got.jpg", "medium": "https://example.com/got-m.jpg"}
		}`))
	}))
	defer srv.Close()

	p := NewTVMaze(nil)
	p.baseURL = srv.URL

	res, ok, err := p.Lookup(context.Background(), Query{Title: "game of thrones", IsTV: true, Season: 1, Episode: 1})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if !ok {
		t.Fatal("expected a match")
	}
	if res.Title != "Game of Thrones" || res.Year != 2011 || res.ExternalID != "82" {
		t.Errorf("unexpected result: %+v", res)
	}
	if res.PosterURL != "https://example.com/got.jpg" {
		t.Errorf("poster = %q", res.PosterURL)
	}
	if res.Overview != "Seven noble families fight." {
		t.Errorf("overview not stripped of HTML: %q", res.Overview)
	}
	if res.Season != 1 || res.Episode != 1 {
		t.Errorf("season/ep should echo query: %d/%d", res.Season, res.Episode)
	}
}

func TestTVMazeMissAndNonTV(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	p := NewTVMaze(nil)
	p.baseURL = srv.URL

	// 404 -> definitive miss, no error.
	_, ok, err := p.Lookup(context.Background(), Query{Title: "nonexistent show", IsTV: true})
	if err != nil || ok {
		t.Errorf("expected clean miss, got ok=%v err=%v", ok, err)
	}

	// Non-TV query -> miss without even calling out.
	_, ok, err = p.Lookup(context.Background(), Query{Title: "a movie", IsTV: false})
	if err != nil || ok {
		t.Errorf("non-TV should be a miss, got ok=%v err=%v", ok, err)
	}
}
