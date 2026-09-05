package openapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// specDoc is a minimal view of the OpenAPI document sufficient to validate its
// structure and enumerate its paths.
type specDoc struct {
	OpenAPI string                            `yaml:"openapi"`
	Info    struct{ Title, Version string }   `yaml:"info"`
	Paths   map[string]map[string]interface{} `yaml:"paths"`
}

func parseSpec(t *testing.T) specDoc {
	t.Helper()
	var doc specDoc
	if err := yaml.Unmarshal(Spec(), &doc); err != nil {
		t.Fatalf("spec is not valid YAML: %v", err)
	}
	return doc
}

func TestSpecStructure(t *testing.T) {
	doc := parseSpec(t)
	if !strings.HasPrefix(doc.OpenAPI, "3.") {
		t.Errorf("openapi version = %q, want 3.x", doc.OpenAPI)
	}
	if doc.Info.Title == "" || doc.Info.Version == "" {
		t.Errorf("info.title/version missing: %+v", doc.Info)
	}
	if len(doc.Paths) == 0 {
		t.Fatal("spec declares no paths")
	}
}

// expectedOps mirrors the REST routes registered in
// internal/api/rest.(*API).Routes(), with the /api/v1 base stripped and
// {id}/{guid} templating kept. This is the drift guard: if a route is added or
// removed in rest.go without updating openapi.yaml, this test fails and forces
// the spec to be maintained (#138).
var expectedOps = map[string][]string{
	"/login":                          {"post"},
	"/health":                         {"get"},
	"/ready":                          {"get"},
	"/setup/status":                   {"get"},
	"/setup":                          {"post"},
	"/me":                             {"get"},
	"/categories":                     {"get"},
	"/releases":                       {"get"},
	"/releases/{guid}":                {"get"},
	"/releases/{guid}/nzb":            {"get"},
	"/apikeys":                        {"get", "post"},
	"/apikeys/{id}":                   {"delete"},
	"/admin/groups":                   {"get", "post"},
	"/admin/groups/bulk":              {"post"},
	"/admin/groups/{id}":              {"patch", "delete"},
	"/admin/groups/{id}/backfill":     {"put"},
	"/admin/groups/{id}/scan-config":  {"put"},
	"/admin/users":                    {"get", "post"},
	"/admin/users/{id}":               {"delete"},
	"/admin/servers":                  {"get", "post"},
	"/admin/servers/{id}":             {"put", "delete"},
	"/admin/health":                   {"get"},
	"/admin/schedule":                 {"get", "put"},
	"/admin/scan":                     {"post"},
	"/admin/backfill":                 {"post"},
	"/admin/postprocess":              {"post"},
	"/admin/postprocess/retry-failed": {"post"},
	"/admin/jobs":                     {"get"},
	"/admin/jobs/{id}":                {"get"},
	"/admin/jobs/{id}/cancel":         {"post"},
	"/admin/segments/backfill":        {"post"},
	"/admin/retention/preview":        {"get"},
	"/admin/retention/prune":          {"post"},
	"/admin/status":                   {"get"},
	"/admin/stats":                    {"get"},
	"/admin/logs":                     {"get"},
	"/admin/events":                   {"get"},
	"/admin/overview":                 {"get"},
	"/admin/discover":                 {"get"},
	"/admin/notifications":            {"get"},
	"/admin/capacity":                 {"get"},
	"/admin/diagnostics":              {"get"},
	"/admin/search/reindex":           {"post"},
}

func TestSpecCoversRESTRoutes(t *testing.T) {
	doc := parseSpec(t)
	for path, methods := range expectedOps {
		item, ok := doc.Paths[path]
		if !ok {
			t.Errorf("spec is missing path %q (registered in rest.Routes)", path)
			continue
		}
		for _, m := range methods {
			if _, ok := item[m]; !ok {
				t.Errorf("spec path %q is missing operation %q", path, m)
			}
		}
	}
}

func TestSpecHasNoStaleRESTPaths(t *testing.T) {
	doc := parseSpec(t)
	for path := range doc.Paths {
		if _, ok := expectedOps[path]; !ok {
			t.Errorf("spec declares path %q that is not a known REST route", path)
		}
	}
}

func TestSpecHandler(t *testing.T) {
	rec := httptest.NewRecorder()
	SpecHandler(rec, httptest.NewRequest(http.MethodGet, "/api/v1/openapi.yaml", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/yaml" {
		t.Errorf("content-type = %q", ct)
	}
	if !strings.Contains(rec.Body.String(), "openapi:") {
		t.Errorf("body does not look like the spec")
	}
}

func TestDocsHandler(t *testing.T) {
	rec := httptest.NewRecorder()
	DocsHandler(rec, httptest.NewRequest(http.MethodGet, "/api/v1/docs", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("content-type = %q", ct)
	}
	if !strings.Contains(rec.Body.String(), "redoc") {
		t.Errorf("docs page missing redoc")
	}
}
