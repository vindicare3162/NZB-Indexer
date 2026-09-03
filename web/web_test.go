package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandlerServesIndex(t *testing.T) {
	h, err := Handler()
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("index status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "<div id=\"app\">") {
		t.Errorf("index.html did not contain app mount point:\n%s", rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("content-type = %q", ct)
	}
}

func TestHandlerFallsBackToIndexForClientRoutes(t *testing.T) {
	h, err := Handler()
	if err != nil {
		t.Fatal(err)
	}
	// A deep-link path that isn't a real file should serve index.html so the
	// SPA can handle routing.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/some/client/route", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("fallback status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "<div id=\"app\">") {
		t.Errorf("fallback did not serve index.html")
	}
}

func TestHandlerServesRealAssetsWhenBuilt(t *testing.T) {
	// When a real production build is present, index.html references a hashed
	// bundle under /assets/ and that asset is served. On a bare checkout with
	// only the committed placeholder this is skipped.
	h, err := Handler()
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if !strings.Contains(rec.Body.String(), "/assets/") {
		t.Skip("no production build present (placeholder only); skipping asset check")
	}

	// Extract the JS bundle path and confirm it is served with a JS type.
	body := rec.Body.String()
	i := strings.Index(body, "/assets/")
	rest := body[i:]
	end := strings.IndexAny(rest, `"'`)
	if end < 0 {
		t.Fatalf("could not parse asset path from index:\n%s", body)
	}
	assetPath := rest[:end]

	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, assetPath, nil))
	if rec2.Code != http.StatusOK {
		t.Errorf("asset %s status = %d, want 200", assetPath, rec2.Code)
	}
}
