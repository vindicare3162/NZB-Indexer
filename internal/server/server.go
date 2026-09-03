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
	if active, err := st.GetActiveServer(ctx); err == nil {
		poolCfg = serverToNNTPConfig(active, cfg.NNTP.ConnectTimeout)
		logger.Info("using configured news server", "name", active.Name, "host", active.Host)
	}
	pool := nntp.New(poolCfg)
	defer pool.Close()

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
		// Process releases in parallel, using up to half the NNTP connection
		// budget so scanning/assembly still have connections available.
		Concurrency: ppConcurrency(cfg.NNTP.MaxConns),
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
	wrk := worker.New(st, sc, asm, builder, pp, logger, worker.Options{
		ScanInterval:        cfg.Scan.Interval,
		DownstreamInterval:  cfg.Scan.DownstreamInterval,
		BuildInterval:       cfg.Scan.BuildInterval,
		PostProcessInterval: cfg.Scan.PostProcessInterval,
		EnableBackfill:      enableBackfill,
	})

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
	srvMgr := &serverManager{store: st, pool: pool, connectTimeout: cfg.NNTP.ConnectTimeout, log: logger}
	discovery := newDiscoveryService(pool, time.Hour)
	restAPI := rest.New(st, nzbGen, authSvc, authSvc, wrk, srvMgr, logs, discovery, logger)

	spa, err := web.Handler()
	if err != nil {
		return fmt.Errorf("spa handler: %w", err)
	}

	mux := http.NewServeMux()
	// Newznab API, protected by API-key auth.
	mux.Handle("/api", authSvc.RequireAPIKey(nnHandler))
	mux.Handle("/api/", authSvc.RequireAPIKey(nnHandler))
	// JSON REST API for the SPA.
	mux.Handle("/api/v1/", restAPI.Routes())
	// Everything else: the embedded SPA.
	mux.Handle("/", spa)

	srv := &http.Server{
		Addr:         cfg.Server.ListenAddr,
		Handler:      mux,
		ReadTimeout:  cfg.Server.ReadTimeout,
		WriteTimeout: cfg.Server.WriteTimeout,
	}

	// 7. Run background goroutines: worker, rate-limit cleanup, and the HTTP
	// server. Shut down gracefully when ctx is cancelled.
	workerCtx, cancelWorker := context.WithCancel(context.Background())
	go wrk.Run(workerCtx)
	go authSvc.CleanupLoop(workerCtx, 10*time.Minute)

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

// ppConcurrency chooses how many releases to post-process in parallel: about
// half the NNTP connection budget, bounded to [1, 4], so scanning and assembly
// still have connections available.
func ppConcurrency(maxConns int) int {
	n := maxConns / 2
	if n < 1 {
		n = 1
	}
	if n > 4 {
		n = 4
	}
	return n
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

// serverManager applies the active news server to the live NNTP pool. It
// implements rest.ServerManager.
type serverManager struct {
	store          *store.Store
	pool           *nntp.Pool
	connectTimeout time.Duration
	log            *slog.Logger
}

// ApplyActive reloads the active server and reconfigures the pool. When no
// server is enabled, the pool keeps its current configuration.
func (m *serverManager) ApplyActive(ctx context.Context) error {
	active, err := m.store.GetActiveServer(ctx)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			m.log.Warn("no enabled news server configured; pool unchanged")
			return nil
		}
		return err
	}
	m.pool.Reconfigure(serverToNNTPConfig(active, m.connectTimeout))
	m.log.Info("applied news server to pool", "name", active.Name, "host", active.Host)
	return nil
}
