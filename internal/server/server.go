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
func Run(ctx context.Context, cfg config.Config, logger *slog.Logger) error {
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

	// 2. NNTP connection pool.
	pool := nntp.New(nntp.Config{
		Host:           cfg.NNTP.Host,
		Port:           cfg.NNTP.Port,
		TLS:            cfg.NNTP.TLS,
		Username:       cfg.NNTP.Username,
		Password:       cfg.NNTP.Password,
		MaxConns:       cfg.NNTP.MaxConns,
		ConnectTimeout: cfg.NNTP.ConnectTimeout,
		MaxRetries:     3,
		RetryBackoff:   500 * time.Millisecond,
	})
	defer pool.Close()

	// 3. Pipeline stages.
	sc := scanner.New(pool, st, logger, scanner.Options{
		BatchSize:           int64(cfg.Scan.BatchSize),
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
	})
	nzbGen := nzb.NewGenerator(st)

	// 4. Worker scheduler (also serves as the REST JobController).
	wrk := worker.New(st, sc, asm, builder, pp, logger, worker.Options{
		ScanInterval:   cfg.Scan.Interval,
		EnableBackfill: cfg.Scan.BackfillDays > 0 || cfg.Scan.BackfillMaxArticles > 0,
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
	restAPI := rest.New(st, nzbGen, authSvc, authSvc, wrk, logger)

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
