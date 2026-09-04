// Package openapi serves and embeds the goindex OpenAPI 3 specification (#138).
// The spec at openapi.yaml documents the REST API; it is embedded so it ships
// with the binary and is served at GET /api/v1/openapi.yaml. A small HTML page
// at GET /api/v1/docs renders it with Redoc for human browsing.
package openapi

import (
	_ "embed"
	"net/http"
)

//go:embed openapi.yaml
var specYAML []byte

// Spec returns the raw embedded OpenAPI YAML document.
func Spec() []byte { return specYAML }

// SpecHandler serves the OpenAPI YAML document. The spec is public API
// documentation, so it requires no authentication.
func SpecHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/yaml")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_, _ = w.Write(specYAML)
}

// docsHTML renders the spec with Redoc (loaded from a CDN). It points at the
// sibling openapi.yaml endpoint.
const docsHTML = `<!DOCTYPE html>
<html>
  <head>
    <title>goindex API</title>
    <meta charset="utf-8"/>
    <meta name="viewport" content="width=device-width, initial-scale=1">
    <style>body { margin: 0; padding: 0; }</style>
  </head>
  <body>
    <redoc spec-url="/api/v1/openapi.yaml"></redoc>
    <script src="https://cdn.redoc.ly/redoc/latest/bundles/redoc.standalone.js"></script>
  </body>
</html>`

// DocsHandler serves an HTML page that renders the spec with Redoc.
func DocsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(docsHTML))
}
