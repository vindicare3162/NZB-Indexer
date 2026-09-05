package metrics

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// pipelineCollector implements prometheus.Collector, pulling the pipeline and
// worker snapshots on each scrape rather than maintaining background state.
// This keeps the metrics consistent with the live system and adds no work
// unless something scrapes /metrics.
type pipelineCollector struct {
	p Providers

	// Descriptors are created once; values are produced per scrape.
	partsTotal              *prometheus.Desc
	partsUnassigned         *prometheus.Desc
	binariesTotal           *prometheus.Desc
	binariesComplete        *prometheus.Desc
	binariesUnreleased      *prometheus.Desc
	releasesTotal           *prometheus.Desc
	releasesByPP            *prometheus.Desc
	releasesFailedExhausted *prometheus.Desc

	workerRunning         *prometheus.Desc
	workerCycles          *prometheus.Desc
	workerArticlesPulled  *prometheus.Desc
	workerPartsInserted   *prometheus.Desc
	workerBinariesTouched *prometheus.Desc
	workerReleasesCreated *prometheus.Desc
	workerReleasesRenamed *prometheus.Desc
	workerNFOsFound       *prometheus.Desc

	scrapeErrors *prometheus.Desc

	authCacheHits      *prometheus.Desc
	authCacheMisses    *prometheus.Desc
	authCacheEvictions *prometheus.Desc
	authCacheSize      *prometheus.Desc

	nntpPoolOpen         *prometheus.Desc
	nntpPoolIdle         *prometheus.Desc
	nntpPoolMax          *prometheus.Desc
	dbPoolTotal          *prometheus.Desc
	dbPoolIdle           *prometheus.Desc
	dbPoolAcquired       *prometheus.Desc
	dbPoolMax            *prometheus.Desc
	dbPoolEmptyAcquires  *prometheus.Desc
	dbPoolAcquireWaitSec *prometheus.Desc
	dbReservedAPIConns   *prometheus.Desc
	dbPipelineBudget     *prometheus.Desc

	// Group freshness (#129): bounded scalar aggregates, no per-group series.
	groupActive          *prometheus.Desc
	groupBehind          *prometheus.Desc
	groupMaxLag          *prometheus.Desc
	groupTotalLag        *prometheus.Desc
	groupFailing         *prometheus.Desc
	groupMaxFailures     *prometheus.Desc
	groupOldestSuccessS  *prometheus.Desc
	groupNeverScanned    *prometheus.Desc

	// NNTP provider health (#129): labeled by bounded server name.
	nntpProviderCircuit    *prometheus.Desc
	nntpProviderConsecFail *prometheus.Desc
	nntpProviderFailures   *prometheus.Desc
	nntpProviderSuccess    *prometheus.Desc
	nntpProviderOpens      *prometheus.Desc
	nntpProviderPoolOpen   *prometheus.Desc
	nntpProviderPoolIdle   *prometheus.Desc
}

func newPipelineCollector(p Providers) *pipelineCollector {
	d := func(name, help string, labels ...string) *prometheus.Desc {
		return prometheus.NewDesc(name, help, labels, nil)
	}
	return &pipelineCollector{
		p:                       p,
		partsTotal:              d("goindex_parts_total", "Estimated total parts rows."),
		partsUnassigned:         d("goindex_parts_unassigned", "Estimated parts not yet folded into a binary (assembler backlog)."),
		binariesTotal:           d("goindex_binaries_total", "Total binaries."),
		binariesComplete:        d("goindex_binaries_complete", "Complete binaries."),
		binariesUnreleased:      d("goindex_binaries_unreleased", "Complete but not-yet-released binaries."),
		releasesTotal:           d("goindex_releases_total", "Total releases."),
		releasesByPP:            d("goindex_releases_by_pp_status", "Releases by post-processing status.", "status"),
		releasesFailedExhausted: d("goindex_releases_failed_exhausted", "Releases permanently failed (retry budget exhausted)."),
		workerRunning:           d("goindex_worker_running", "1 when a post-process cycle is in progress, else 0."),
		workerCycles:            d("goindex_worker_cycles_total", "Completed post-process cycles."),
		workerArticlesPulled:    d("goindex_worker_articles_pulled_total", "Articles pulled by scans."),
		workerPartsInserted:     d("goindex_worker_parts_inserted_total", "Parts inserted by scans."),
		workerBinariesTouched:   d("goindex_worker_binaries_touched_total", "Binaries touched by the assembler."),
		workerReleasesCreated:   d("goindex_worker_releases_created_total", "Releases created by the builder."),
		workerReleasesRenamed:   d("goindex_worker_releases_renamed_total", "Releases renamed by post-processing."),
		workerNFOsFound:         d("goindex_worker_nfos_found_total", "NFOs recovered by post-processing."),
		scrapeErrors:            d("goindex_metrics_scrape_errors_total", "Errors gathering pipeline metrics on scrape."),
		authCacheHits:           d("goindex_apikey_cache_hits_total", "API-key auth cache hits."),
		authCacheMisses:         d("goindex_apikey_cache_misses_total", "API-key auth cache misses."),
		authCacheEvictions:      d("goindex_apikey_cache_evictions_total", "API-key auth cache evictions."),
		authCacheSize:           d("goindex_apikey_cache_size", "API-key auth cache current entry count."),

		nntpPoolOpen:         d("goindex_nntp_pool_open_connections", "NNTP pool live connections (idle + in use)."),
		nntpPoolIdle:         d("goindex_nntp_pool_idle_connections", "NNTP pool idle (ready-to-use) connections."),
		nntpPoolMax:          d("goindex_nntp_pool_max_connections", "NNTP pool connection ceiling."),
		dbPoolTotal:          d("goindex_db_pool_total_connections", "PostgreSQL pool total connections."),
		dbPoolIdle:           d("goindex_db_pool_idle_connections", "PostgreSQL pool idle connections."),
		dbPoolAcquired:       d("goindex_db_pool_acquired_connections", "PostgreSQL pool connections currently checked out."),
		dbPoolMax:            d("goindex_db_pool_max_connections", "PostgreSQL pool connection ceiling."),
		dbPoolEmptyAcquires:  d("goindex_db_pool_empty_acquires_total", "PostgreSQL acquisitions that waited on an empty pool (saturation)."),
		dbPoolAcquireWaitSec: d("goindex_db_pool_acquire_wait_seconds_total", "Cumulative time spent waiting to acquire a PostgreSQL connection."),
		dbReservedAPIConns:   d("goindex_db_reserved_api_connections", "PostgreSQL connections reserved for the API/control plane (#117)."),
		dbPipelineBudget:     d("goindex_db_pipeline_budget_connections", "PostgreSQL connections the pipeline may use concurrently (#117)."),

		groupActive:         d("goindex_groups_active", "Active groups being indexed."),
		groupBehind:         d("goindex_groups_behind", "Active groups with positive forward lag."),
		groupMaxLag:         d("goindex_group_lag_max_articles", "Largest forward lag (articles) across active groups."),
		groupTotalLag:       d("goindex_group_lag_total_articles", "Summed forward lag (articles) across active groups."),
		groupFailing:        d("goindex_groups_failing", "Active groups with at least one consecutive scan failure."),
		groupMaxFailures:    d("goindex_group_consecutive_failures_max", "Largest consecutive-failure count across active groups."),
		groupOldestSuccessS: d("goindex_group_oldest_success_age_seconds", "Age of the least-recently successful active group's last success."),
		groupNeverScanned:   d("goindex_groups_never_scanned", "Active groups that have never scanned successfully."),

		nntpProviderCircuit:    d("goindex_nntp_provider_circuit_state", "NNTP provider circuit state (0=closed, 1=half-open, 2=open).", "server"),
		nntpProviderConsecFail: d("goindex_nntp_provider_consecutive_failures", "NNTP provider consecutive connection failures.", "server"),
		nntpProviderFailures:   d("goindex_nntp_provider_failures_total", "NNTP provider total connection failures.", "server"),
		nntpProviderSuccess:    d("goindex_nntp_provider_success_total", "NNTP provider total successful operations.", "server"),
		nntpProviderOpens:      d("goindex_nntp_provider_circuit_opens_total", "NNTP provider circuit-open events.", "server"),
		nntpProviderPoolOpen:   d("goindex_nntp_provider_pool_open_connections", "NNTP provider live connections.", "server"),
		nntpProviderPoolIdle:   d("goindex_nntp_provider_pool_idle_connections", "NNTP provider idle connections.", "server"),
	}
}

func (c *pipelineCollector) Describe(ch chan<- *prometheus.Desc) {
	prometheus.DescribeByCollect(c, ch)
}

func (c *pipelineCollector) Collect(ch chan<- prometheus.Metric) {
	var scrapeErr float64

	if c.p.Pipeline != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		snap, err := c.p.Pipeline(ctx)
		cancel()
		if err != nil {
			scrapeErr++
		} else {
			g := func(desc *prometheus.Desc, v float64, labels ...string) {
				ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, v, labels...)
			}
			g(c.partsTotal, snap.PartsTotal)
			g(c.partsUnassigned, snap.PartsUnassigned)
			g(c.binariesTotal, snap.BinariesTotal)
			g(c.binariesComplete, snap.BinariesComplete)
			g(c.binariesUnreleased, snap.BinariesUnreleased)
			g(c.releasesTotal, snap.ReleasesTotal)
			g(c.releasesFailedExhausted, snap.ReleasesFailedExhausted)
			for status, v := range snap.ReleasesByPPStatus {
				g(c.releasesByPP, v, status)
			}
		}
	}

	if c.p.Worker != nil {
		w := c.p.Worker()
		gauge := func(desc *prometheus.Desc, v float64) {
			ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, v)
		}
		counter := func(desc *prometheus.Desc, v float64) {
			ch <- prometheus.MustNewConstMetric(desc, prometheus.CounterValue, v)
		}
		gauge(c.workerRunning, w.Running)
		counter(c.workerCycles, w.Cycles)
		counter(c.workerArticlesPulled, w.ArticlesPulled)
		counter(c.workerPartsInserted, w.PartsInserted)
		counter(c.workerBinariesTouched, w.BinariesTouched)
		counter(c.workerReleasesCreated, w.ReleasesCreated)
		counter(c.workerReleasesRenamed, w.ReleasesRenamed)
		counter(c.workerNFOsFound, w.NFOsFound)
	}

	if c.p.AuthCache != nil {
		a := c.p.AuthCache()
		ch <- prometheus.MustNewConstMetric(c.authCacheHits, prometheus.CounterValue, a.Hits)
		ch <- prometheus.MustNewConstMetric(c.authCacheMisses, prometheus.CounterValue, a.Misses)
		ch <- prometheus.MustNewConstMetric(c.authCacheEvictions, prometheus.CounterValue, a.Evictions)
		ch <- prometheus.MustNewConstMetric(c.authCacheSize, prometheus.GaugeValue, a.Size)
	}

	if c.p.Pools != nil {
		p := c.p.Pools()
		gauge := func(desc *prometheus.Desc, v float64) {
			ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, v)
		}
		counter := func(desc *prometheus.Desc, v float64) {
			ch <- prometheus.MustNewConstMetric(desc, prometheus.CounterValue, v)
		}
		gauge(c.nntpPoolOpen, p.NNTPOpen)
		gauge(c.nntpPoolIdle, p.NNTPIdle)
		gauge(c.nntpPoolMax, p.NNTPMax)
		gauge(c.dbPoolTotal, p.DBTotal)
		gauge(c.dbPoolIdle, p.DBIdle)
		gauge(c.dbPoolAcquired, p.DBAcquired)
		gauge(c.dbPoolMax, p.DBMax)
		counter(c.dbPoolEmptyAcquires, p.DBEmptyAcquires)
		counter(c.dbPoolAcquireWaitSec, p.DBAcquireWaitSeconds)
		gauge(c.dbReservedAPIConns, p.DBReservedAPIConns)
		gauge(c.dbPipelineBudget, p.DBPipelineBudget)
	}

	if c.p.GroupHealth != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		gh, err := c.p.GroupHealth(ctx)
		cancel()
		if err != nil {
			scrapeErr++
		} else {
			g := func(desc *prometheus.Desc, v float64) {
				ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, v)
			}
			g(c.groupActive, gh.ActiveGroups)
			g(c.groupBehind, gh.GroupsBehind)
			g(c.groupMaxLag, gh.MaxLag)
			g(c.groupTotalLag, gh.TotalLag)
			g(c.groupFailing, gh.GroupsFailing)
			g(c.groupMaxFailures, gh.MaxConsecutiveFailures)
			g(c.groupOldestSuccessS, gh.OldestSuccessAgeSeconds)
			g(c.groupNeverScanned, gh.GroupsNeverScanned)
		}
	}

	if c.p.NNTPHealth != nil {
		for _, h := range c.p.NNTPHealth() {
			gauge := func(desc *prometheus.Desc, v float64) {
				ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, v, h.Name)
			}
			counter := func(desc *prometheus.Desc, v float64) {
				ch <- prometheus.MustNewConstMetric(desc, prometheus.CounterValue, v, h.Name)
			}
			gauge(c.nntpProviderCircuit, h.CircuitState)
			gauge(c.nntpProviderConsecFail, h.ConsecutiveFailures)
			counter(c.nntpProviderFailures, h.TotalFailures)
			counter(c.nntpProviderSuccess, h.TotalSuccess)
			counter(c.nntpProviderOpens, h.CircuitOpens)
			gauge(c.nntpProviderPoolOpen, h.PoolOpen)
			gauge(c.nntpProviderPoolIdle, h.PoolIdle)
		}
	}

	ch <- prometheus.MustNewConstMetric(c.scrapeErrors, prometheus.CounterValue, scrapeErr)
}
