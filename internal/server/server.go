// Package server wires the goindex components (store, NNTP pool, pipeline,
// auth, APIs, and embedded SPA) into a single HTTP server with a background
// worker, and manages graceful startup and shutdown.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/vindicare/goindex/internal/api/newznab"
	"github.com/vindicare/goindex/internal/api/rest"
	"github.com/vindicare/goindex/internal/assembler"
	"github.com/vindicare/goindex/internal/auth"
	"github.com/vindicare/goindex/internal/config"
	"github.com/vindicare/goindex/internal/logbuf"
	"github.com/vindicare/goindex/internal/metrics"
	"github.com/vindicare/goindex/internal/nntp"
	"github.com/vindicare/goindex/internal/postprocess"
	"github.com/vindicare/goindex/internal/release"
	"github.com/vindicare/goindex/internal/nzb"
	"github.com/vindicare/goindex/internal/scanner"
	"github.com/vindicare/goindex/internal/store"
	"github.com/vindicare/goindex/internal/worker"
	"github.com/vindicare/goindex/web"
)

// Run builds and runs the full server until ctx is cancelled, then shuts down
// gracefully. It applies migrations on startup.
func Run(ctx context.Context, cfg config.Config, logger *slog.Logger, logs *logbuf.Buffer) error {
	// 1. Database: migrate then open a pool.
	dsn := cfg.Database.DSNString()
	logger.Info("applying database migrations")
	if err := store.Migrate(dsn); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	st, err := store.Open(ctx, dsn, cfg.Database.MaxConns)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()

	// 2. NNTP connection pool. Servers are managed in the database and editable
	// from the UI. On first run (no servers configured) seed one from the
	// startup config so existing deployments keep working. The active server
	// (highest priority, enabled) is applied to the pool.
	if err := seedServerFromConfig(ctx, st, cfg, logger); err != nil {
		return fmt.Errorf("seed server: %w", err)
	}
	// A default connection config used when no DB server is configured yet.
	poolCfg := nntp.Config{
		Host:           cfg.NNTP.Host,
		Port:           cfg.NNTP.Port,
		TLS:            cfg.NNTP.TLS,
		Username:       cfg.NNTP.Username,
		Password:       cfg.NNTP.Password,
		MaxConns:       cfg.NNTP.MaxConns,
		ConnectTimeout: cfg.NNTP.ConnectTimeout,
		MaxRetries:     3,
		RetryBackoff:   500 * time.Millisecond,
	}
	// Build a failover pool over ALL enabled servers by priority (#128): the
	// highest-priority healthy provider serves each request, with per-provider
	// circuit breaking and automatic failover/recovery.
	endpoints := buildEndpoints(ctx, st, cfg, poolCfg)
	pool := nntp.NewFailover(endpoints, nntp.FailoverOptions{
		FailureThreshold: cfg.NNTP.CircuitFailureThreshold,
		Cooldown:         cfg.NNTP.CircuitCooldown,
	})
	defer pool.Close()
	if active, err := st.GetActiveServer(ctx); err == nil {
		logger.Info("using configured news server", "name", active.Name, "host", active.Host,
			"servers", len(endpoints))
	}

	// Effective NNTP connection capacity: the highest-priority (active) server's
	// connection limit, so scan/post-process worker counts match the real
	// provider budget.
	effectiveNNTPConns := poolCfg.MaxConns
	if len(endpoints) > 0 {
		effectiveNNTPConns = endpoints[0].Config.MaxConns
	}

	// Resource budgeting (#117): size the NNTP-bound pipeline stages against
	// BOTH the effective NNTP capacity and the PostgreSQL pool, reserving
	// headroom on the DB pool for the HTTP API and admin control plane so
	// pipeline load cannot starve them. Explicit operator overrides are honoured
	// but flagged when they overcommit the DB pipeline budget.
	budget := computeBudget(effectiveNNTPConns, cfg.Database.MaxConns, cfg.Database.ReservedConns,
		cfg.Scan.Concurrency, 0)

	// 3. Pipeline stages.
	sc := scanner.New(pool, st, logger, scanner.Options{
		BatchSize:           int64(cfg.Scan.BatchSize),
		ForwardMaxArticles:  int64(cfg.Scan.ForwardMaxArticles),
		BackfillDays:        cfg.Scan.BackfillDays,
		BackfillMaxArticles: int64(cfg.Scan.BackfillMaxArticles),
	})
	asm := assembler.New(st, logger, assembler.Options{
		BatchLimit: 1000,
		StaleAfter: 14 * 24 * time.Hour,
	})
	builder := release.New(st, logger, release.Options{BatchLimit: 1000})
	pp := postprocess.New(pool, st, logger, postprocess.Options{
		BatchLimit:         200,
		MaxFetchPerRelease: 4,
		FetchTimeout:       30 * time.Second,
		// Process releases in parallel within the resource budget (#117), which
		// respects both the NNTP pool and the DB pipeline budget (leaving API
		// headroom).
		Concurrency: budget.PostProcessWorkers,
	})
	nzbGen := nzb.NewGenerator(st)

	// 4. Worker scheduler (also serves as the REST JobController). Backfill runs
	// on the schedule when a global setting is configured or any group has a
	// per-group backfill target (runtime-added targets are also reachable via
	// the manual per-group Backfill action immediately).
	enableBackfill := cfg.Scan.BackfillDays > 0 || cfg.Scan.BackfillMaxArticles > 0
	if !enableBackfill {
		if has, err := st.AnyGroupHasBackfillTarget(ctx); err == nil && has {
			enableBackfill = true
		}
	}
	// Schedule intervals: config/env provide the defaults, but a value set at
	// runtime from the admin UI (persisted in the settings table) takes
	// precedence so it survives restarts.
	wopts := worker.Options{
		ScanInterval:        cfg.Scan.Interval,
		DownstreamInterval:  cfg.Scan.DownstreamInterval,
		BuildInterval:       cfg.Scan.BuildInterval,
		PostProcessInterval: cfg.Scan.PostProcessInterval,
		EnableBackfill:      enableBackfill,
	}
	applyPersistedSchedule(ctx, st, &wopts, logger)
	wopts.EnrichInterval = cfg.Metadata.Interval
	wopts.ScanConcurrency = budget.ScanWorkers
	// Backlog-aware scheduling for the downstream loops (#125).
	wopts.AdaptiveMinInterval = cfg.Scan.AdaptiveMinInterval
	logger.Info("effective concurrency sizing",
		"nntp_max_conns", budget.NNTPMaxConns,
		"db_max_conns", budget.DBMaxConns,
		"db_reserved_api_conns", budget.ReservedAPIConns,
		"db_pipeline_budget", budget.DBPipelineBudget,
		"scan_concurrency", budget.ScanWorkers,
		"postprocess_concurrency", budget.PostProcessWorkers)
	if budget.Overcommit {
		logger.Warn("pipeline concurrency overcommits the database pipeline budget; "+
			"the API/control plane may block on connection acquisition under load. "+
			"Consider raising database.max_conns or lowering scan.concurrency.",
			"scan_concurrency", budget.ScanWorkers,
			"postprocess_concurrency", budget.PostProcessWorkers,
			"db_pipeline_budget", budget.DBPipelineBudget)
	}

	// Optional metadata enrichment. Disabled by default; when enabled it uses
	// the keyless TVMaze provider unless another is configured. A nil enricher
	// leaves the pipeline unchanged.
	enricher := buildEnricher(st, cfg, logger)
	wrk := worker.New(st, sc, asm, builder, pp, enricher, logger, wopts)
	// Persistent pipeline jobs (#113): record manual triggers as jobs, and mark
	// any jobs left running by a previous process as interrupted (recovery).
	wrk.SetJobStore(st)
	// Per-group scan progress/error reporting (#114): persist each group's
	// scan/backfill outcome as passes run.
	wrk.SetGroupScanRecorder(st)
	if n, err := st.MarkInterruptedJobs(ctx); err != nil {
		logger.Warn("failed to recover interrupted jobs", "err", err)
	} else if n > 0 {
		logger.Info("recovered interrupted jobs", "count", n)
	}

	// 5. Auth.
	tokens, err := auth.NewTokenIssuer(cfg.Auth.JWTSecret, cfg.Auth.SessionTTL)
	if err != nil {
		return fmt.Errorf("auth setup: %w", err)
	}
	limiter := auth.NewRateLimiter(cfg.Auth.RateLimitWindow)
	authSvc := auth.NewService(st, tokens, limiter, cfg.Auth.DefaultRateLimit)

	// 6. HTTP handlers.
	nnHandler := newznab.NewHandler(st, nzbGen, newznab.Config{
		BaseURL:      cfg.Server.BaseURL,
		MaxLimit:     100,
		DefaultLimit: 100,
	})
	srvMgr := &serverManager{store: st, pool: pool, cfg: cfg, connectTimeout: cfg.NNTP.ConnectTimeout, log: logger}
	discovery := newDiscoveryService(pool, time.Hour)
	restAPI := rest.New(st, nzbGen, authSvc, authSvc, scheduleAdapter{wrk}, srvMgr, logs, discovery, logger)
	restAPI.SetSystemProbe(systemProbe{
		pool: pool, store: st, jwtSecret: cfg.Auth.JWTSecret,
		nntpMaxConns: budget.NNTPMaxConns,
		scanWorkers:  budget.ScanWorkers,
		ppWorkers:    budget.PostProcessWorkers,
	})
	// Raw-part retention window/batch defaults for the admin retention endpoints.
	restAPI.SetRetention(cfg.Retention.Days, cfg.Retention.BatchSize, cfg.Retention.MaxBatchesPerRun)
	// Per-group health thresholds for the admin group listing (#127).
	restAPI.SetGroupHealthThresholds(store.GroupHealthThresholds{
		LagWarn:       cfg.Health.LagWarn,
		LagError:      cfg.Health.LagError,
		StaleWarn:     cfg.Health.StaleWarn,
		StaleError:    cfg.Health.StaleError,
		FailuresWarn:  cfg.Health.FailuresWarn,
		FailuresError: cfg.Health.FailuresError,
	})
	// Live log streaming for the admin Server-Sent Events endpoint (#121).
	restAPI.SetLogStreamer(logs)

	spa, err := web.Handler()
	if err != nil {
		return fmt.Errorf("spa handler: %w", err)
	}

	// Prometheus metrics: HTTP instruments plus a pipeline/worker collector
	// evaluated on scrape from the store stats and worker snapshot.
	met := metrics.New(metrics.Providers{
		Pipeline: func(ctx context.Context) (metrics.PipelineSnapshot, error) {
			return pipelineSnapshot(ctx, st)
		},
		Worker: func() metrics.WorkerSnapshot {
			return workerSnapshot(wrk.MetricsSnapshot())
		},
		AuthCache: func() metrics.AuthCacheSnapshot {
			s := authSvc.APIKeyCacheStats()
			return metrics.AuthCacheSnapshot{
				Hits: float64(s.Hits), Misses: float64(s.Misses),
				Evictions: float64(s.Evictions), Size: float64(s.Size),
			}
		},
		Pools: func() metrics.PoolSnapshot {
			nOpen, nIdle := pool.Stats()
			db := st.PoolStats()
			return metrics.PoolSnapshot{
				NNTPOpen: float64(nOpen), NNTPIdle: float64(nIdle), NNTPMax: float64(budget.NNTPMaxConns),
				DBTotal: float64(db.Total), DBIdle: float64(db.Idle),
				DBAcquired: float64(db.Acquired), DBMax: float64(db.Max),
				DBEmptyAcquires:      float64(db.EmptyAcquires),
				DBAcquireWaitSeconds: db.AcquireWaitSec,
				DBReservedAPIConns:   float64(budget.ReservedAPIConns),
				DBPipelineBudget:     float64(budget.DBPipelineBudget),
			}
		},
	})

	mux := http.NewServeMux()
	// Newznab API, protected by API-key auth.
	mux.Handle("/api", authSvc.RequireAPIKey(nnHandler))
	mux.Handle("/api/", authSvc.RequireAPIKey(nnHandler))
	// JSON REST API for the SPA.
	mux.Handle("/api/v1/", restAPI.Routes())
	// Prometheus scrape endpoint (unauthenticated by convention; exposes only
	// operational counters, no secrets).
	mux.Handle("/metrics", met.Handler())
	// Everything else: the embedded SPA.
	mux.Handle("/", spa)

	srv := &http.Server{
		Addr:         cfg.Server.ListenAddr,
		Handler:      met.Middleware(accessLog(logger, mux)),
		ReadTimeout:  cfg.Server.ReadTimeout,
		WriteTimeout: cfg.Server.WriteTimeout,
	}

	// 7. Run background goroutines: worker, rate-limit cleanup, and the HTTP
	// server. Shut down gracefully when ctx is cancelled.
	workerCtx, cancelWorker := context.WithCancel(context.Background())
	go wrk.Run(workerCtx)
	go authSvc.CleanupLoop(workerCtx, 10*time.Minute)
	go runJobCleanupLoop(workerCtx, st, logger)
	if cfg.Retention.Enabled && cfg.Retention.Days > 0 {
		go runRetentionLoop(workerCtx, st, cfg.Retention, logger)
	}

	serverErr := make(chan error, 1)
	go func() {
		logger.Info("http server listening", "addr", cfg.Server.ListenAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	select {
	case <-ctx.Done():
		logger.Info("shutdown signal received")
	case err := <-serverErr:
		cancelWorker()
		return fmt.Errorf("http server: %w", err)
	}

	// Graceful shutdown: stop accepting connections, then stop the worker.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Warn("http shutdown error", "err", err)
	}
	cancelWorker()
	logger.Info("goindex stopped")
	return nil
}

// runJobCleanupLoop periodically prunes terminal job history older than 7 days
// (#113) so the jobs table doesn't grow unbounded. Cancellable via ctx.
func runJobCleanupLoop(ctx context.Context, st *store.Store, logger *slog.Logger) {
	const retain = 7 * 24 * time.Hour
	ticker := time.NewTicker(6 * time.Hour)
	defer ticker.Stop()
	cleanup := func() {
		if n, err := st.CleanupOldJobs(ctx, retain); err != nil {
			logger.Warn("job history cleanup failed", "err", err)
		} else if n > 0 {
			logger.Debug("cleaned up old jobs", "count", n)
		}
	}
	cleanup()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cleanup()
		}
	}
}

// runRetentionLoop periodically prunes raw parts for reconstructable, released,
// fully-processed releases older than the retention window (#118). It runs on
// its own goroutine, bounded per run and cancellable via ctx, so it never holds
// an unbounded transaction and stops promptly on shutdown. Results are logged.
func runRetentionLoop(ctx context.Context, st *store.Store, rc config.RetentionConfig, logger *slog.Logger) {
	interval := rc.Interval
	if interval <= 0 {
		interval = 6 * time.Hour
	}
	olderThan := time.Duration(rc.Days) * 24 * time.Hour
	logger.Info("raw-part retention enabled",
		"days", rc.Days, "interval", interval, "batch_size", rc.BatchSize,
		"max_batches_per_run", rc.MaxBatchesPerRun)

	runOnce := func() {
		deleted, err := st.PruneRetainedPartsAll(ctx, olderThan, rc.BatchSize, rc.MaxBatchesPerRun)
		if err != nil {
			logger.Error("retention prune failed", "err", err)
			return
		}
		if deleted > 0 {
			logger.Info("retention prune completed", "days", rc.Days, "parts_deleted", deleted)
		}
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	runOnce() // an initial pass at startup
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			runOnce()
		}
	}
}

// buildEndpoints returns the failover endpoints for all ENABLED news servers,
// ordered by priority. When no server is configured it falls back to the
// startup NNTP config as a single endpoint so a bare env/YAML deployment still
// works.
func buildEndpoints(ctx context.Context, st *store.Store, cfg config.Config, fallback nntp.Config) []nntp.EndpointConfig {
	servers, err := st.ListServers(ctx)
	if err == nil && len(servers) > 0 {
		var eps []nntp.EndpointConfig
		for _, s := range servers {
			if !s.Enabled {
				continue
			}
			eps = append(eps, nntp.EndpointConfig{
				ID:       s.ID,
				Name:     s.Name,
				Priority: s.Priority,
				Config:   serverToNNTPConfig(s, cfg.NNTP.ConnectTimeout),
			})
		}
		if len(eps) > 0 {
			return eps
		}
	}
	if fallback.Host == "" {
		return nil
	}
	return []nntp.EndpointConfig{{ID: 0, Name: "default", Priority: 0, Config: fallback}}
}

// serverToNNTPConfig converts a stored news server into an nntp.Config.
func serverToNNTPConfig(s store.Server, connectTimeout time.Duration) nntp.Config {
	return nntp.Config{
		Host:           s.Host,
		Port:           s.Port,
		TLS:            s.TLS,
		Username:       s.Username,
		Password:       s.Password,
		MaxConns:       s.MaxConns,
		ConnectTimeout: connectTimeout,
		MaxRetries:     3,
		RetryBackoff:   500 * time.Millisecond,
	}
}

// seedServerFromConfig inserts a news server from the startup NNTP config when
// none is configured yet, so existing env/YAML-based deployments migrate
// seamlessly to DB-managed servers.
func seedServerFromConfig(ctx context.Context, st *store.Store, cfg config.Config, logger *slog.Logger) error {
	n, err := st.CountServers(ctx)
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	if cfg.NNTP.Host == "" {
		return nil // nothing to seed
	}
	pw := cfg.NNTP.Password
	_, err = st.CreateServer(ctx, store.ServerInput{
		Name:     "default",
		Host:     cfg.NNTP.Host,
		Port:     cfg.NNTP.Port,
		TLS:      cfg.NNTP.TLS,
		Username: cfg.NNTP.Username,
		Password: &pw,
		MaxConns: cfg.NNTP.MaxConns,
		Priority: 0,
		Enabled:  true,
	})
	if err != nil {
		return err
	}
	logger.Info("seeded news server from startup config", "host", cfg.NNTP.Host)
	return nil
}

// serverManager rebuilds the failover pool's endpoints from the DB-managed
// servers when they change. It implements rest.ServerManager.
type serverManager struct {
	store          *store.Store
	pool           *nntp.FailoverPool
	cfg            config.Config
	connectTimeout time.Duration
	log            *slog.Logger
}

// ApplyActive reloads all enabled servers and rebuilds the failover pool's
// endpoints (#128) so provider changes (add/edit/priority/max-conns/enable)
// take effect live. Existing servers keep their pool + circuit state; removed
// ones are closed.
func (m *serverManager) ApplyActive(ctx context.Context) error {
	fallback := nntp.Config{
		Host: m.cfg.NNTP.Host, Port: m.cfg.NNTP.Port, TLS: m.cfg.NNTP.TLS,
		Username: m.cfg.NNTP.Username, Password: m.cfg.NNTP.Password,
		MaxConns: m.cfg.NNTP.MaxConns, ConnectTimeout: m.cfg.NNTP.ConnectTimeout,
		MaxRetries: 3, RetryBackoff: 500 * time.Millisecond,
	}
	eps := buildEndpoints(ctx, m.store, m.cfg, fallback)
	if len(eps) == 0 {
		m.log.Warn("no enabled news server configured; pool unchanged")
		return nil
	}
	m.pool.SetEndpoints(eps)
	name, _, _ := m.pool.ActiveServer()
	m.log.Info("applied news servers to failover pool", "servers", len(eps), "active", name)
	return nil
}
