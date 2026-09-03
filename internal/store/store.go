package store

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5" // registers the pgx5:// migrate driver
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5/pgxpool"
)

// migrationsFS embeds the SQL migration files so the binary is self-contained.
//
//go:embed migrations/*.sql
var migrationsFS embed.FS

// ErrNotFound is returned by lookups when no matching row exists.
var ErrNotFound = errors.New("store: not found")

// Store wraps a pgx connection pool and provides typed data access.
type Store struct {
	pool *pgxpool.Pool
}

// Open creates a connection pool from the given DSN and verifies connectivity.
// maxConns bounds the pool size. Callers must Close the returned Store.
func Open(ctx context.Context, dsn string, maxConns int) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}
	if maxConns > 0 {
		cfg.MaxConns = int32(maxConns)
	}

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return &Store{pool: pool}, nil
}

// Pool exposes the underlying connection pool for packages that need direct
// query access (scanner, assembler, etc.).
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// Close releases all pooled connections.
func (s *Store) Close() {
	if s.pool != nil {
		s.pool.Close()
	}
}

// Ping verifies the database is reachable.
func (s *Store) Ping(ctx context.Context) error {
	return s.pool.Ping(ctx)
}

// Migrate applies all pending up migrations against the given DSN. It is safe
// to call on every startup; already-applied migrations are skipped.
func Migrate(dsn string) error {
	m, err := newMigrator(dsn)
	if err != nil {
		return err
	}
	defer closeMigrator(m)

	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("apply migrations: %w", err)
	}
	return nil
}

// MigrateDown rolls back all migrations. Intended for tests and maintenance.
func MigrateDown(dsn string) error {
	m, err := newMigrator(dsn)
	if err != nil {
		return err
	}
	defer closeMigrator(m)

	if err := m.Down(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("roll back migrations: %w", err)
	}
	return nil
}

// MigrationVersion reports the current schema version and whether the schema
// is in a dirty (failed-migration) state.
func MigrationVersion(dsn string) (version uint, dirty bool, err error) {
	m, e := newMigrator(dsn)
	if e != nil {
		return 0, false, e
	}
	defer closeMigrator(m)

	v, d, err := m.Version()
	if err != nil && !errors.Is(err, migrate.ErrNilVersion) {
		return 0, false, err
	}
	return v, d, nil
}

// newMigrator wires the embedded iofs source to the pgx database driver.
func newMigrator(dsn string) (*migrate.Migrate, error) {
	src, err := iofs.New(migrationsFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("load embedded migrations: %w", err)
	}
	// golang-migrate's pgx/v5 driver expects a pgx5:// scheme URL.
	m, err := migrate.NewWithSourceInstance("iofs", src, ensurePgx5Scheme(dsn))
	if err != nil {
		return nil, fmt.Errorf("create migrator: %w", err)
	}
	return m, nil
}

func closeMigrator(m *migrate.Migrate) {
	srcErr, dbErr := m.Close()
	_ = srcErr
	_ = dbErr
}

// ensurePgx5Scheme returns a pgx5://-schemed URL that the golang-migrate
// pgx/v5 driver understands. It accepts either a postgres:// URL or a libpq
// key=value DSN (as produced by DatabaseConfig.DSNString with discrete
// fields), converting the latter into a URL because golang-migrate requires a
// scheme.
func ensurePgx5Scheme(dsn string) string {
	switch {
	case strings.HasPrefix(dsn, "postgres://"):
		return "pgx5://" + strings.TrimPrefix(dsn, "postgres://")
	case strings.HasPrefix(dsn, "postgresql://"):
		return "pgx5://" + strings.TrimPrefix(dsn, "postgresql://")
	case strings.HasPrefix(dsn, "pgx5://"):
		return dsn
	default:
		// Assume a libpq key=value DSN; convert to a URL.
		return keyValueToURL(dsn)
	}
}

// keyValueToURL converts a libpq "key=value key=value" DSN into a
// pgx5://user:pass@host:port/dbname?param=... URL.
func keyValueToURL(dsn string) string {
	kv := map[string]string{}
	for _, field := range strings.Fields(dsn) {
		if i := strings.IndexByte(field, '='); i > 0 {
			kv[field[:i]] = field[i+1:]
		}
	}

	host := kv["host"]
	if host == "" {
		host = "localhost"
	}
	port := kv["port"]
	if port == "" {
		port = "5432"
	}

	u := url.URL{
		Scheme: "pgx5",
		Host:   host + ":" + port,
		Path:   "/" + kv["dbname"],
	}
	if user := kv["user"]; user != "" {
		if pw, ok := kv["password"]; ok {
			u.User = url.UserPassword(user, pw)
		} else {
			u.User = url.User(user)
		}
	}
	q := url.Values{}
	for _, k := range []string{"sslmode", "connect_timeout", "search_path"} {
		if v, ok := kv[k]; ok {
			q.Set(k, v)
		}
	}
	u.RawQuery = q.Encode()
	return u.String()
}
