// Command goindex is a self-hosted NZB indexer: it scans Usenet newsgroup
// headers, assembles multi-part binaries into releases, post-processes them,
// and serves both a Newznab-compatible API and a JSON API for a web UI.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/vindicare/goindex/internal/auth"
	"github.com/vindicare/goindex/internal/config"
	"github.com/vindicare/goindex/internal/logbuf"
	"github.com/vindicare/goindex/internal/nntp"
	"github.com/vindicare/goindex/internal/server"
	"github.com/vindicare/goindex/internal/store"
)

// version is overridden at build time via -ldflags.
var version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	// Subcommand dispatch: `goindex <cmd> [flags]`.
	if len(args) > 0 {
		switch args[0] {
		case "migrate":
			return runMigrate(args[1:])
		case "nntp-test":
			return runNNTPTest(args[1:])
		case "user":
			return runUser(args[1:])
		case "serve":
			return runServe(args[1:])
		case "healthcheck":
			return runHealthcheck(args[1:])
		}
	}

	fs := flag.NewFlagSet("goindex", flag.ContinueOnError)
	configPath := fs.String("config", envOr("GOINDEX_CONFIG", ""), "path to YAML config file (optional; env vars still apply)")
	showVersion := fs.Bool("version", false, "print version and exit")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if *showVersion {
		fmt.Println("goindex", version)
		return nil
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}

	logger := newLogger(cfg.LogLevel)
	logger.Info("goindex starting", "version", version)
	logger.Info("configuration loaded (secrets redacted)")
	for _, line := range strings.Split(cfg.Summary(), "\n") {
		logger.Info(line)
	}

	logger.Info("no subcommand given; use 'goindex serve' to start the server")
	logger.Info("available subcommands: serve, migrate, nntp-test, user")
	return nil
}

// runServe starts the full HTTP server and background worker, shutting down
// gracefully on SIGINT/SIGTERM.
func runServe(args []string) error {
	fs := flag.NewFlagSet("goindex serve", flag.ContinueOnError)
	configPath := fs.String("config", envOr("GOINDEX_CONFIG", ""), "path to YAML config file")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	// A bounded in-memory log buffer captures recent entries for the admin UI,
	// alongside the usual stderr output.
	logs := logbuf.New(2000)
	logger := newLoggerWithBuffer(cfg.LogLevel, logs)
	logger.Info("goindex starting", "version", version)
	for _, line := range strings.Split(cfg.Summary(), "\n") {
		logger.Info(line)
	}

	// Cancel the root context on SIGINT/SIGTERM for graceful shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return server.Run(ctx, cfg, logger, logs)
}

// newLogger builds a slog logger at the configured level, writing text to
// stderr.
func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	h := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl})
	return slog.New(h)
}

// newLoggerWithBuffer builds a logger that writes to stderr and also captures
// records into the given in-memory buffer for the admin log view.
func newLoggerWithBuffer(level string, buf *logbuf.Buffer) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	stderr := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl})
	multi := logbuf.NewMultiHandler(stderr, buf.NewHandler())
	return slog.New(multi)
}

// runMigrate applies or rolls back database migrations.
//
//	goindex migrate            # apply all pending up migrations
//	goindex migrate -down      # roll everything back
//	goindex migrate -version   # print current schema version
func runMigrate(args []string) error {
	fs := flag.NewFlagSet("goindex migrate", flag.ContinueOnError)
	configPath := fs.String("config", envOr("GOINDEX_CONFIG", ""), "path to YAML config file")
	down := fs.Bool("down", false, "roll back all migrations")
	showVer := fs.Bool("version", false, "print current schema version")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	dsn := cfg.Database.DSNString()
	logger := newLogger(cfg.LogLevel)

	switch {
	case *showVer:
		v, dirty, err := store.MigrationVersion(dsn)
		if err != nil {
			return err
		}
		logger.Info("schema version", "version", v, "dirty", dirty)
		return nil
	case *down:
		logger.Warn("rolling back all migrations")
		if err := store.MigrateDown(dsn); err != nil {
			return err
		}
		logger.Info("migrations rolled back")
		return nil
	default:
		logger.Info("applying migrations")
		if err := store.Migrate(dsn); err != nil {
			return err
		}
		v, dirty, err := store.MigrationVersion(dsn)
		if err != nil {
			return err
		}
		logger.Info("migrations applied", "version", v, "dirty", dirty)
		return nil
	}
}

// runNNTPTest connects to the configured provider, authenticates, selects a
// group, and prints its article range. It is a connectivity smoke test.
//
//	goindex nntp-test [-group alt.binaries.xyz]
func runNNTPTest(args []string) error {
	fs := flag.NewFlagSet("goindex nntp-test", flag.ContinueOnError)
	configPath := fs.String("config", envOr("GOINDEX_CONFIG", ""), "path to YAML config file")
	group := fs.String("group", "", "newsgroup to test (defaults to first configured scan group)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	logger := newLogger(cfg.LogLevel)

	target := *group
	if target == "" {
		if len(cfg.Scan.Groups) == 0 {
			return fmt.Errorf("no group specified and no scan.groups configured")
		}
		target = cfg.Scan.Groups[0]
	}

	pool := nntp.New(nntp.Config{
		Host:           cfg.NNTP.Host,
		Port:           cfg.NNTP.Port,
		TLS:            cfg.NNTP.TLS,
		Username:       cfg.NNTP.Username,
		Password:       cfg.NNTP.Password,
		MaxConns:       cfg.NNTP.MaxConns,
		ConnectTimeout: cfg.NNTP.ConnectTimeout,
		MaxRetries:     2,
		RetryBackoff:   500 * time.Millisecond,
	})
	defer pool.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	logger.Info("connecting to NNTP provider", "host", cfg.NNTP.Host, "port", cfg.NNTP.Port, "tls", cfg.NNTP.TLS)
	info, err := pool.SelectGroupInfo(ctx, target)
	if err != nil {
		return fmt.Errorf("select group %q: %w", target, err)
	}
	logger.Info("group selected",
		"group", info.Name,
		"low", info.Low,
		"high", info.High,
		"count", info.Count,
	)
	return nil
}

// runUser manages user accounts and API keys.
//
//	goindex user add -username U -password P [-admin] [-apikey]
//	goindex user apikey -username U [-label L]
//	goindex user list
func runUser(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: goindex user <add|apikey|list> [flags]")
	}
	sub := args[0]
	fs := flag.NewFlagSet("goindex user "+sub, flag.ContinueOnError)
	configPath := fs.String("config", envOr("GOINDEX_CONFIG", ""), "path to YAML config file")
	username := fs.String("username", "", "username")
	password := fs.String("password", "", "password (add only)")
	admin := fs.Bool("admin", false, "grant admin role (add only)")
	withKey := fs.Bool("apikey", false, "also mint an API key (add only)")
	label := fs.String("label", "", "API key label (apikey only)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	logger := newLogger(cfg.LogLevel)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	st, err := store.Open(ctx, cfg.Database.DSNString(), cfg.Database.MaxConns)
	if err != nil {
		return err
	}
	defer st.Close()

	switch sub {
	case "add":
		if *username == "" || *password == "" {
			return fmt.Errorf("-username and -password are required")
		}
		hash, err := auth.HashPassword(*password)
		if err != nil {
			return err
		}
		role := store.RoleUser
		if *admin {
			role = store.RoleAdmin
		}
		u, err := st.CreateUser(ctx, store.CreateUserInput{
			Username: *username, PasswordHash: hash, Role: role,
		})
		if err != nil {
			return err
		}
		logger.Info("user created", "id", u.ID, "username", u.Username, "role", u.Role)
		if *withKey {
			key, err := auth.GenerateAPIKey()
			if err != nil {
				return err
			}
			if _, err := st.CreateAPIKey(ctx, u.ID, key, "default"); err != nil {
				return err
			}
			logger.Info("api key created", "username", u.Username, "apikey", key)
		}
		return nil

	case "apikey":
		if *username == "" {
			return fmt.Errorf("-username is required")
		}
		u, err := st.GetUserByUsername(ctx, *username)
		if err != nil {
			return err
		}
		key, err := auth.GenerateAPIKey()
		if err != nil {
			return err
		}
		lbl := *label
		if lbl == "" {
			lbl = "default"
		}
		if _, err := st.CreateAPIKey(ctx, u.ID, key, lbl); err != nil {
			return err
		}
		logger.Info("api key created", "username", u.Username, "label", lbl, "apikey", key)
		return nil

	case "list":
		users, err := st.ListUsers(ctx)
		if err != nil {
			return err
		}
		for _, u := range users {
			logger.Info("user", "id", u.ID, "username", u.Username, "role", u.Role, "active", u.Active)
		}
		return nil

	default:
		return fmt.Errorf("unknown user subcommand %q", sub)
	}
}

// runHealthcheck probes the local server's health endpoint and exits non-zero
// on failure. It is used by the Docker HEALTHCHECK so the runtime image needs
// no curl/wget.
func runHealthcheck(args []string) error {
	fs := flag.NewFlagSet("goindex healthcheck", flag.ContinueOnError)
	addr := fs.String("addr", envOr("GOINDEX_HEALTHCHECK_ADDR", "http://127.0.0.1:8080"), "base URL to probe")
	if err := fs.Parse(args); err != nil {
		return err
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(strings.TrimRight(*addr, "/") + "/api/v1/health")
	if err != nil {
		return fmt.Errorf("healthcheck: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthcheck: status %d", resp.StatusCode)
	}
	return nil
}

func envOr(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return fallback
}
