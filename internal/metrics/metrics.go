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
	Pipeline  func(ctx context.Context) (PipelineSnapshot, error)
	Worker    func() WorkerSnapshot
	AuthCache func() AuthCacheSnapshot
	// Pools reports NNTP + PostgreSQL pool utilisation and saturation so
	// operators can see when the pipeline is contending for connections and
	// whether the API/control plane has headroom (#117). Nil omits the metrics.
	Pools func() PoolSnapshot
	// GroupHealth reports aggregate group-freshness signals (#129). Bounded
	// scalar gauges only (no per-group series). Nil omits the metrics.
	GroupHealth func(ctx context.Context) (GroupHealthSnapshot, error)
	// NNTPHealth reports per-provider circuit/failure/pool state (#129). Labeled
	// by the configured server name, which is bounded (operators configure a
	// handful of servers). Nil omits the metrics.
	NNTPHealth func() []ProviderHealthSnapshot
}

// GroupHealthSnapshot is the aggregate group-freshness view for metrics (#129).
// It carries only bounded scalars, never per-group rows.
type GroupHealthSnapshot struct {
	ActiveGroups            float64
	GroupsBehind            float64
	MaxLag                  float64
	TotalLag                float64
	GroupsFailing           float64
	MaxConsecutiveFailures  float64
	OldestSuccessAgeSeconds float64
	GroupsNeverScanned      float64
}

// ProviderHealthSnapshot is one NNTP provider's observable health for metrics
// (#129). Name is used as a bounded label.
type ProviderHealthSnapshot struct {
	Name string
	// CircuitState is 0 (closed), 1 (half-open), or 2 (open).
	CircuitState        float64
	ConsecutiveFailures float64
	TotalFailures       float64
	TotalSuccess        float64
	CircuitOpens        float64
	PoolOpen            float64
	PoolIdle            float64
}

// AuthCacheSnapshot reports API-key auth cache activity.
type AuthCacheSnapshot struct {
	Hits      float64
	Misses    float64
	Evictions float64
	Size      float64
}

// PoolSnapshot reports connection-pool utilisation and saturation for the two
// pools the system contends for (#117).
type PoolSnapshot struct {
	// NNTP connection pool.
	NNTPOpen float64 // live connections (idle + in use)
	NNTPIdle float64 // ready-to-use connections
	NNTPMax  float64 // configured ceiling

	// PostgreSQL (pgx) pool.
	DBTotal    float64 // total connections currently in the pool
	DBIdle     float64 // idle connections
	DBAcquired float64 // connections currently checked out
	DBMax      float64 // pool ceiling
	// DBEmptyAcquires is the cumulative count of acquisitions that had to wait
	// for a connection because the pool was empty (saturation signal).
	DBEmptyAcquires float64
	// DBAcquireWaitSeconds is the cumulative time spent waiting to acquire a
	// connection (pgx AcquireDuration), a direct pool-wait signal.
	DBAcquireWaitSeconds float64

	// Budget (static, from startup sizing).
	DBReservedAPIConns float64 // connections reserved for API/control plane
	DBPipelineBudget   float64 // connections the pipeline may use concurrently
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

	if p.Pipeline != nil || p.Worker != nil || p.AuthCache != nil ||
		p.Pools != nil || p.GroupHealth != nil || p.NNTPHealth != nil {
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
