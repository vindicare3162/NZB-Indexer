package metrics

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRoutePattern(t *testing.T) {
	cases := map[string]string{
		"/metrics":                                    "/metrics",
		"/api":                                         "/api",
		"/api/":                                        "/api",
		"/api/v1/login":                                "/api/v1/login",
		"/api/v1/health":                               "/api/v1/health",
		"/api/v1/releases":                             "/api/v1/releases",
		"/api/v1/releases/abc-123-guid":                "/api/v1/releases/:guid",
		"/api/v1/releases/abc-123-guid/nzb":            "/api/v1/releases/:guid/nzb",
		"/api/v1/apikeys":                              "/api/v1/apikeys",
		"/api/v1/apikeys/42":                           "/api/v1/apikeys/:id",
		"/api/v1/admin/groups":                         "/api/v1/admin/groups",
		"/api/v1/admin/groups/bulk":                    "/api/v1/admin/groups/bulk",
		"/api/v1/admin/groups/7":                       "/api/v1/admin/groups/:id",
		"/api/v1/admin/groups/7/backfill":              "/api/v1/admin/groups/:id/backfill",
		"/api/v1/admin/servers/3":                      "/api/v1/admin/servers/:id",
		"/api/v1/admin/users/9":                        "/api/v1/admin/users/:id",
		"/api/v1/admin/postprocess/retry-failed":       "/api/v1/admin/postprocess/retry-failed",
		"/api/v1/admin/schedule":                       "/api/v1/admin/schedule",
		"/":                                            "/",
		"/assets/index-abc.js":                         "spa",
	}
	for path, want := range cases {
		if got := RoutePattern(path); got != want {
			t.Errorf("RoutePattern(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestMiddlewareAndHandlerExposeMetrics(t *testing.T) {
	m := New(Providers{
		Pipeline: func(context.Context) (PipelineSnapshot, error) {
			return PipelineSnapshot{
				PartsTotal:         1000,
				PartsUnassigned:    200,
				ReleasesTotal:      50,
				ReleasesByPPStatus: map[string]float64{"pending": 40, "done": 10},
			}, nil
		},
		Worker: func() WorkerSnapshot {
			return WorkerSnapshot{Running: 1, Cycles: 5, ReleasesCreated: 12}
		},
	})

	// Drive a request through the middleware so HTTP metrics have data.
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/releases/some-guid", nil)
	m.Middleware(next).ServeHTTP(httptest.NewRecorder(), req)

	// Scrape /metrics.
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("metrics status = %d", rec.Code)
	}
	body := rec.Body.String()

	for _, want := range []string{
		`goindex_http_requests_total{method="GET",route="/api/v1/releases/:guid",status="200"} 1`,
		"goindex_http_request_duration_seconds_bucket",
		"goindex_parts_total 1000",
		"goindex_parts_unassigned 200",
		"goindex_releases_total 50",
		`goindex_releases_by_pp_status{status="pending"} 40`,
		"goindex_worker_running 1",
		"goindex_worker_releases_created_total 12",
		"goindex_metrics_scrape_errors_total 0",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics output missing %q", want)
		}
	}
}

func TestScrapeErrorCounted(t *testing.T) {
	m := New(Providers{
		Pipeline: func(context.Context) (PipelineSnapshot, error) {
			return PipelineSnapshot{}, context.DeadlineExceeded
		},
	})
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if !strings.Contains(rec.Body.String(), "goindex_metrics_scrape_errors_total 1") {
		t.Errorf("expected a scrape error to be counted:\n%s", rec.Body.String())
	}
}
