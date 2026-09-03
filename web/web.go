// Package web embeds the built Svelte SPA and serves it, with SPA-style
// fallback to index.html for client-side routes.
package web

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

// distFS embeds the built frontend. The dist directory is produced by
// `npm run build` in this directory. A committed placeholder ensures the
// embed directive always has at least one file to embed.
//
//go:embed all:dist
var distFS embed.FS

// Handler returns an http.Handler that serves the embedded SPA. Requests for
// existing static files are served directly; any other path falls back to
// index.html so client-side (hash) routing works and deep links resolve.
func Handler() (http.Handler, error) {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		return nil, err
	}
	fileServer := http.FileServer(http.FS(sub))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Serve the file if it exists; otherwise serve index.html.
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p == "" {
			p = "index.html"
		}
		if _, err := fs.Stat(sub, p); err != nil {
			// Not found: fall back to index.html for SPA routing.
			r2 := new(http.Request)
			*r2 = *r
			r2.URL.Path = "/"
			serveIndex(w, sub)
			return
		}
		fileServer.ServeHTTP(w, r)
	}), nil
}

// serveIndex writes the embedded index.html.
func serveIndex(w http.ResponseWriter, sub fs.FS) {
	data, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		http.Error(w, "frontend not built", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(data)
}
