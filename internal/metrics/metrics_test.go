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

func TestGroupHealthAndNNTPHealthMetrics(t *testing.T) {
	m := New(Providers{
		GroupHealth: func(context.Context) (GroupHealthSnapshot, error) {
			return GroupHealthSnapshot{
				ActiveGroups: 12, GroupsBehind: 3, MaxLag: 4200, TotalLag: 9000,
				GroupsFailing: 1, MaxConsecutiveFailures: 2,
				OldestSuccessAgeSeconds: 7200, GroupsNeverScanned: 4,
			}, nil
		},
		NNTPHealth: func() []ProviderHealthSnapshot {
			return []ProviderHealthSnapshot{
				{Name: "primary", CircuitState: 0, TotalSuccess: 100, TotalFailures: 2, PoolOpen: 5, PoolIdle: 3},
				{Name: "backup", CircuitState: 2, ConsecutiveFailures: 5, CircuitOpens: 1},
			}
		},
	})

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("metrics status = %d", rec.Code)
	}
	body := rec.Body.String()

	for _, want := range []string{
		"goindex_groups_active 12",
		"goindex_groups_behind 3",
		"goindex_group_lag_max_articles 4200",
		"goindex_group_lag_total_articles 9000",
		"goindex_groups_failing 1",
		"goindex_group_consecutive_failures_max 2",
		"goindex_group_oldest_success_age_seconds 7200",
		"goindex_groups_never_scanned 4",
		`goindex_nntp_provider_circuit_state{server="primary"} 0`,
		`goindex_nntp_provider_circuit_state{server="backup"} 2`,
		`goindex_nntp_provider_consecutive_failures{server="backup"} 5`,
		`goindex_nntp_provider_success_total{server="primary"} 100`,
		`goindex_nntp_provider_circuit_opens_total{server="backup"} 1`,
		"goindex_metrics_scrape_errors_total 0",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics output missing %q", want)
		}
	}

	// Cardinality guard: exactly one circuit-state series per configured server
	// (the label set is bounded by the server name, never per-group).
	if n := strings.Count(body, "goindex_nntp_provider_circuit_state{"); n != 2 {
		t.Errorf("circuit-state series count = %d, want 2 (one per provider)", n)
	}
}

func TestGroupHealthScrapeErrorCounted(t *testing.T) {
	m := New(Providers{
		GroupHealth: func(context.Context) (GroupHealthSnapshot, error) {
			return GroupHealthSnapshot{}, context.DeadlineExceeded
		},
	})
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if !strings.Contains(rec.Body.String(), "goindex_metrics_scrape_errors_total 1") {
		t.Errorf("expected a scrape error to be counted:\n%s", rec.Body.String())
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
