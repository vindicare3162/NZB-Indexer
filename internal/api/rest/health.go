package rest

import (
	"context"
	"net/http"
	"runtime"
	"time"

	"github.com/vindicare/goindex/internal/store"
)

// SystemProbe supplies host/usenet health facts the REST layer cannot observe
// on its own. The server implements it. A nil probe simply omits those fields.
type SystemProbe interface {
	// NNTPPoolStats reports the open and idle NNTP connection counts.
	NNTPPoolStats() (open, idle int)
	// NewsServerConfigured reports whether an active news server is configured.
	NewsServerConfigured(ctx context.Context) bool
	// DefaultJWTSecret reports whether the JWT secret is still the insecure
	// placeholder default (a misconfiguration worth flagging).
	DefaultJWTSecret() bool
	// Capacity reports the effective NNTP connection ceiling the pool was built
	// with and the derived scan/post-process worker limits, so operators can
	// see how concurrency was sized.
	Capacity() (nntpMaxConns, scanWorkers, postProcessWorkers int)
	// ServerHealth reports per-provider circuit/failover health (#128).
	ServerHealth() []ProviderHealth
}

// startTime records process start for uptime reporting.
var startTime = time.Now()

type healthCheck struct {
	Name    string `json:"name"`
	Status  string `json:"status"` // ok | warn | error
	Message string `json:"message,omitempty"`
}

type processHealth struct {
	Goroutines   int    `json:"goroutines"`
	HeapAllocMB  uint64 `json:"heap_alloc_mb"`
	UptimeSecs   int64  `json:"uptime_secs"`
	GoVersion    string `json:"go_version"`
	NumGC        uint32 `json:"num_gc"`
}

type usenetHealth struct {
	PoolOpen         int  `json:"pool_open"`
	PoolIdle         int  `json:"pool_idle"`
	ServerConfigured bool `json:"server_configured"`
	// Effective capacity: the NNTP connection ceiling the pool was built with
	// (from the active DB-managed server when present), and the derived worker
	// limits sized against it.
	MaxConns           int `json:"max_conns"`
	ScanWorkers        int `json:"scan_workers"`
	PostProcessWorkers int `json:"postprocess_workers"`
	// Providers reports per-server circuit/failover health (#128).
	Providers []ProviderHealth `json:"providers,omitempty"`
}

// ProviderHealth is the circuit/failover health of one NNTP provider (#128).
type ProviderHealth struct {
	Name                string `json:"name"`
	Priority            int    `json:"priority"`
	Circuit             string `json:"circuit"` // closed | open | half-open
	ConsecutiveFailures int    `json:"consecutive_failures"`
	LastError           string `json:"last_error,omitempty"`
	LastErrorKind       string `json:"last_error_kind,omitempty"`
	TotalFailures       int64  `json:"total_failures"`
	TotalSuccess        int64  `json:"total_success"`
	CircuitOpens        int64  `json:"circuit_opens"`
	PoolOpen            int    `json:"pool_open"`
	PoolIdle            int    `json:"pool_idle"`
	MaxConns            int    `json:"max_conns"`
}

type healthResponse struct {
	Status   string          `json:"status"` // overall: ok | warn | error
	Process  processHealth   `json:"process"`
	Database *store.DBHealth `json:"database,omitempty"`
	Usenet   *usenetHealth   `json:"usenet,omitempty"`
	Checks   []healthCheck   `json:"checks"`
}

// handleHealthReport aggregates process, database, and usenet health plus a set
// of "potential issue" checks into one admin report. Overall status is the
// worst individual check.
func (a *API) handleHealthReport(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.buildHealthReport(r.Context()))
}

// buildHealthReport assembles the aggregated health report so it can be reused
// by both the dedicated endpoint and the admin overview envelope.
func (a *API) buildHealthReport(ctx context.Context) healthResponse {
	var checks []healthCheck

	// Process.
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	proc := processHealth{
		Goroutines:  runtime.NumGoroutine(),
		HeapAllocMB: ms.HeapAlloc / (1024 * 1024),
		UptimeSecs:  int64(time.Since(startTime).Seconds()),
		GoVersion:   runtime.Version(),
		NumGC:       ms.NumGC,
	}

	// Database health + reachability.
	var dbHealth *store.DBHealth
	pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	if err := a.store.Ping(pingCtx); err != nil {
		checks = append(checks, healthCheck{Name: "database", Status: "error", Message: "database unreachable"})
	} else {
		checks = append(checks, healthCheck{Name: "database", Status: "ok", Message: "reachable"})
		if h, err := a.store.DatabaseHealth(ctx); err == nil {
			dbHealth = &h
			// Cache hit ratio check (only meaningful once there has been read
			// activity). Below ~0.90 on a warmed DB suggests memory pressure.
			if h.CacheHitRatio >= 0 && h.CacheHitRatio < 0.90 {
				checks = append(checks, healthCheck{
					Name: "db_cache", Status: "warn",
					Message: "low buffer cache hit ratio; consider more shared_buffers/RAM",
				})
			}
		}
	}
	cancel()

	// Usenet + config checks from the system probe.
	var usenet *usenetHealth
	if a.probe != nil {
		open, idle := a.probe.NNTPPoolStats()
		configured := a.probe.NewsServerConfigured(ctx)
		maxConns, scanW, ppW := a.probe.Capacity()
		providers := a.probe.ServerHealth()
		usenet = &usenetHealth{
			PoolOpen: open, PoolIdle: idle, ServerConfigured: configured,
			MaxConns: maxConns, ScanWorkers: scanW, PostProcessWorkers: ppW,
			Providers: providers,
		}
		if !configured {
			checks = append(checks, healthCheck{Name: "news_server", Status: "warn", Message: "no active news server configured"})
		}
		// Flag providers whose circuit is open (isolated by failover, #128).
		var openCircuits int
		for _, ph := range providers {
			if ph.Circuit == "open" {
				openCircuits++
			}
		}
		if openCircuits > 0 {
			status := "warn"
			if len(providers) > 0 && openCircuits >= len(providers) {
				status = "error" // every provider is down
			}
			checks = append(checks, healthCheck{
				Name: "news_provider_circuit", Status: status,
				Message: "one or more NNTP providers are circuit-open (failing); failover is active",
			})
		}
		if a.probe.DefaultJWTSecret() {
			checks = append(checks, healthCheck{Name: "jwt_secret", Status: "warn", Message: "JWT secret is the insecure default; set GOINDEX_AUTH_JWT_SECRET"})
		}
	}

	// Pipeline-derived checks (best-effort).
	if stats, err := a.store.PipelineStatistics(ctx); err == nil {
		if len(stats.Groups) == 0 {
			checks = append(checks, healthCheck{Name: "groups", Status: "warn", Message: "no groups have produced releases yet"})
		}
		if stats.ReleasesFailedExhausted > 0 {
			checks = append(checks, healthCheck{
				Name: "postprocess", Status: "warn",
				Message: "some releases failed post-processing permanently; consider 'Retry failed'",
			})
		}
	}

	return healthResponse{
		Status:   overallStatus(checks),
		Process:  proc,
		Database: dbHealth,
		Usenet:   usenet,
		Checks:   checks,
	}
}

// overallStatus returns the worst status among the checks.
func overallStatus(checks []healthCheck) string {
	status := "ok"
	for _, c := range checks {
		switch c.Status {
		case "error":
			return "error"
		case "warn":
			status = "warn"
		}
	}
	return status
}
