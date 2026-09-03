// Package metrics exposes goindex runtime and pipeline metrics in Prometheus
// text format. It provides an HTTP middleware that records request counters and
// latency histograms with bounded route labels, and a collector that surfaces
// pipeline depth and worker activity on scrape from existing in-process
// sources (no extra database load beyond the cheap pipeline statistics).
package metrics

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// PipelineSnapshot is the pipeline-depth view the collector exposes. It mirrors
// store.PipelineStats but is owned here so the metrics package does not import
// the store or worker packages (the server adapts those into these types).
type PipelineSnapshot struct {
	PartsTotal              float64
	PartsUnassigned         float64
	BinariesTotal           float64
	BinariesComplete        float64
	BinariesUnreleased      float64
	ReleasesTotal           float64
	ReleasesByPPStatus      map[string]float64
	ReleasesFailedExhausted float64
}

// WorkerSnapshot is the worker-activity view the collector exposes.
type WorkerSnapshot struct {
	Running         float64
	Cycles          float64
	ArticlesPulled  float64
	PartsInserted   float64
	BinariesTouched float64
	ReleasesCreated float64
	ReleasesRenamed float64
	NFOsFound       float64
}

// Providers supply the current snapshots at scrape time. Either may be nil,
// in which case that family of metrics is omitted.
type Providers struct {
	Pipeline func(ctx context.Context) (PipelineSnapshot, error)
	Worker   func() WorkerSnapshot
}

// Metrics bundles the registry, HTTP instruments, and the scrape handler.
type Metrics struct {
	reg          *prometheus.Registry
	httpRequests *prometheus.CounterVec
	httpDuration *prometheus.HistogramVec
}

// New builds a metrics registry with the standard Go/process collectors, the
// HTTP instruments, and (when providers are supplied) a pipeline/worker
// collector evaluated on each scrape.
func New(p Providers) *Metrics {
	reg := prometheus.NewRegistry()
	reg.MustRegister(prometheus.NewGoCollector())
	reg.MustRegister(prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}))

	m := &Metrics{
		reg: reg,
		httpRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "goindex_http_requests_total",
			Help: "Total HTTP requests by method, route pattern, and status.",
		}, []string{"method", "route", "status"}),
		httpDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "goindex_http_request_duration_seconds",
			Help:    "HTTP request duration in seconds by method and route pattern.",
			Buckets: prometheus.DefBuckets,
		}, []string{"method", "route"}),
	}
	reg.MustRegister(m.httpRequests, m.httpDuration)

	if p.Pipeline != nil || p.Worker != nil {
		reg.MustRegister(newPipelineCollector(p))
	}
	return m
}

// Handler returns the /metrics HTTP handler for this registry.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{})
}

// Middleware records the request count and duration for each request, using a
// bounded route label so high-cardinality path segments (GUIDs, queries) do not
// explode the metric series.
func (m *Metrics) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		route := RoutePattern(r.URL.Path)
		m.httpRequests.WithLabelValues(r.Method, route, strconv.Itoa(rec.status)).Inc()
		m.httpDuration.WithLabelValues(r.Method, route).Observe(time.Since(start).Seconds())
	})
}

// statusRecorder captures the response status for the middleware.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if !r.wroteHeader {
		r.status = code
		r.wroteHeader = true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.status = http.StatusOK
		r.wroteHeader = true
	}
	return r.ResponseWriter.Write(b)
}
