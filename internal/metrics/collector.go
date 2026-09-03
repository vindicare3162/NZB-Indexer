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

	ch <- prometheus.MustNewConstMetric(c.scrapeErrors, prometheus.CounterValue, scrapeErr)
}
