// Package config loads and validates goindex runtime configuration from
// environment variables and an optional YAML file. Environment variables take
// precedence over YAML values, which in turn override built-in defaults.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the fully resolved runtime configuration for goindex.
type Config struct {
	// Server holds HTTP listener settings.
	Server ServerConfig `yaml:"server"`
	// Database holds PostgreSQL connection settings.
	Database DatabaseConfig `yaml:"database"`
	// NNTP holds the upstream Usenet provider connection settings.
	NNTP NNTPConfig `yaml:"nntp"`
	// Scan holds header-scanning and backfill behaviour.
	Scan ScanConfig `yaml:"scan"`
	// Auth holds authentication settings.
	Auth AuthConfig `yaml:"auth"`
	// Metadata holds optional release metadata-enrichment settings.
	Metadata MetadataConfig `yaml:"metadata"`
	// Retention holds raw-part retention settings.
	Retention RetentionConfig `yaml:"retention"`
	// LogLevel is one of debug, info, warn, error.
	LogLevel string `yaml:"log_level"`
}

// RetentionConfig configures raw-part retention (#118): pruning the raw article
// rows for released, fully post-processed, reconstructable releases older than
// a window. Disabled by default so existing installations are unaffected until
// an operator opts in.
type RetentionConfig struct {
	// Enabled turns on the background retention loop. When false, no parts are
	// ever pruned automatically (the dry-run report and manual admin trigger
	// remain available).
	Enabled bool `yaml:"enabled"`
	// Days is the retention window: only parts of releases older than this many
	// days are eligible for pruning. Must be > 0 when Enabled.
	Days int `yaml:"days"`
	// Interval is how often the retention loop runs when Enabled. Zero uses a
	// sane default (6h).
	Interval time.Duration `yaml:"interval"`
	// BatchSize bounds how many parts are deleted per transaction, so cleanup
	// never holds one unbounded transaction. Zero uses a sane default (5000).
	BatchSize int `yaml:"batch_size"`
	// MaxBatchesPerRun caps how many batches a single automatic run deletes, so
	// a run is time-bounded. Zero means drain fully.
	MaxBatchesPerRun int `yaml:"max_batches_per_run"`
}

// MetadataConfig configures optional release metadata enrichment (matching
// releases to TV shows/movies for cover art and details). Disabled by default
// so existing deployments are unaffected.
type MetadataConfig struct {
	// Enabled turns on the enrichment loop. When false, releases carry no
	// external metadata and no provider requests are made.
	Enabled bool `yaml:"enabled"`
	// Provider selects the metadata source. Currently "tvmaze" (keyless, TV) is
	// supported and is the default when Enabled and Provider is empty.
	Provider string `yaml:"provider"`
	// Interval is how often the enrichment loop runs. Zero uses a sane default.
	Interval time.Duration `yaml:"interval"`
}

// ServerConfig configures the embedded HTTP server.
type ServerConfig struct {
	// ListenAddr is the host:port the server binds to, e.g. ":8080".
	ListenAddr string `yaml:"listen_addr"`
	// BaseURL is the externally reachable base URL, used when generating
	// absolute links (e.g. NZB download URLs in Newznab responses).
	BaseURL string `yaml:"base_url"`
	// ReadTimeout bounds the time to read an entire request.
	ReadTimeout time.Duration `yaml:"read_timeout"`
	// WriteTimeout bounds the time to write a response.
	WriteTimeout time.Duration `yaml:"write_timeout"`
}

// DatabaseConfig configures the PostgreSQL connection pool.
type DatabaseConfig struct {
	// DSN is the full PostgreSQL connection string. If set, it takes
	// precedence over the individual Host/Port/... fields.
	DSN string `yaml:"dsn"`
	// Host is the PostgreSQL server hostname.
	Host string `yaml:"host"`
	// Port is the PostgreSQL server port.
	Port int `yaml:"port"`
	// User is the PostgreSQL user.
	User string `yaml:"user"`
	// Password is the PostgreSQL password.
	Password string `yaml:"password"`
	// Name is the database name.
	Name string `yaml:"name"`
	// SSLMode is the libpq sslmode (disable, require, verify-full, ...).
	SSLMode string `yaml:"ssl_mode"`
	// MaxConns is the maximum number of pooled connections.
	MaxConns int `yaml:"max_conns"`
	// ReservedConns is how many database connections to hold back for the HTTP
	// API and admin control plane so pipeline workers cannot consume the whole
	// pool and starve searches/admin requests (#117). It must be less than
	// MaxConns. A negative value (the default) means "auto-derive from the pool
	// size"; 0 disables the reservation (unsafe under heavy pipeline load).
	ReservedConns int `yaml:"reserved_conns"`
}

// NNTPConfig configures the upstream Usenet provider connection.
type NNTPConfig struct {
	// Host is the NNTP server hostname.
	Host string `yaml:"host"`
	// Port is the NNTP server port (563 for TLS, 119 for plaintext).
	Port int `yaml:"port"`
	// TLS enables an implicit TLS connection.
	TLS bool `yaml:"tls"`
	// Username is the provider account username.
	Username string `yaml:"username"`
	// Password is the provider account password.
	Password string `yaml:"password"`
	// MaxConns bounds the connection pool size (provider connection limit).
	MaxConns int `yaml:"max_conns"`
	// CircuitFailureThreshold is the number of consecutive connection failures
	// that opens a provider's circuit breaker (#128), isolating it from the
	// failover rotation until a recovery probe succeeds. 0 uses a default.
	CircuitFailureThreshold int `yaml:"circuit_failure_threshold"`
	// CircuitCooldown is how long a provider's circuit stays open before a
	// recovery probe is attempted. 0 uses a default.
	CircuitCooldown time.Duration `yaml:"circuit_cooldown"`
	// ConnectTimeout bounds dialing a new connection.
	ConnectTimeout time.Duration `yaml:"connect_timeout"`
}

// ScanConfig configures the header scanner and backfill.
type ScanConfig struct {
	// Groups is the list of newsgroups to index.
	Groups []string `yaml:"groups"`
	// BatchSize is the number of articles requested per XOVER call.
	BatchSize int `yaml:"batch_size"`
	// Concurrency is how many groups are scanned in parallel within one pass.
	// It shares the NNTP connection budget (nntp.max_conns) with
	// post-processing, so real parallelism is still capped by the pool. 0 means
	// a sensible default derived from the pool size; 1 forces the original
	// sequential behaviour.
	Concurrency int `yaml:"concurrency"`
	// Interval is how often forward scans run.
	Interval time.Duration `yaml:"interval"`
	// DownstreamInterval is how often the assemble/build loop runs,
	// independently of scanning so a long scan cannot starve it. Zero defaults
	// to Interval.
	DownstreamInterval time.Duration `yaml:"downstream_interval"`
	// BuildInterval is how often the release-build loop runs, independently of
	// assembly so a large parts backlog cannot starve release promotion. Zero
	// defaults to DownstreamInterval.
	BuildInterval time.Duration `yaml:"build_interval"`
	// PostProcessInterval is how often the post-process loop (obfuscated-name
	// recovery, NFO capture) runs. It runs independently of both scanning and
	// assemble/build, so a large parts backlog cannot starve name recovery.
	// Zero defaults to DownstreamInterval.
	PostProcessInterval time.Duration `yaml:"postprocess_interval"`
	// ForwardMaxArticles caps how many articles a single forward-scan pass
	// ingests per group before yielding, so a firehose group cannot monopolise
	// a cycle. The watermark is persisted so the next pass resumes. Zero means
	// unbounded.
	ForwardMaxArticles int `yaml:"forward_max_articles"`
	// BackfillDays limits how far back a backfill walks, in days. Zero
	// disables date-based backfill.
	BackfillDays int `yaml:"backfill_days"`
	// BackfillMaxArticles caps how many articles a single backfill pass
	// walks backwards. Zero means unlimited (bounded only by BackfillDays).
	BackfillMaxArticles int `yaml:"backfill_max_articles"`
}

// AuthConfig configures authentication.
type AuthConfig struct {
	// JWTSecret signs session tokens for the SPA. Required in production.
	JWTSecret string `yaml:"jwt_secret"`
	// SessionTTL is how long an issued session token is valid.
	SessionTTL time.Duration `yaml:"session_ttl"`
	// DefaultRateLimit is the default per-API-key request limit per window.
	DefaultRateLimit int `yaml:"default_rate_limit"`
	// RateLimitWindow is the rate-limit window duration.
	RateLimitWindow time.Duration `yaml:"rate_limit_window"`
}

// Default returns a Config populated with sensible defaults. Values that must
// be provided by the operator (NNTP host/credentials, DB connection, JWT
// secret) are intentionally left empty so validation can flag them.
func Default() Config {
	return Config{
		Server: ServerConfig{
			ListenAddr:   ":8080",
			ReadTimeout:  30 * time.Second,
			WriteTimeout: 60 * time.Second,
		},
		Database: DatabaseConfig{
			Host:          "localhost",
			Port:          5432,
			User:          "goindex",
			Name:          "goindex",
			SSLMode:       "disable",
			MaxConns:      10,
			ReservedConns: -1, // -1 = auto-derive API/control-plane headroom
		},
		Metadata: MetadataConfig{
			Enabled:  false,
			Provider: "tvmaze",
			Interval: 30 * time.Minute,
		},
		Retention: RetentionConfig{
			Enabled:          false, // opt-in; conservative default for existing installs
			Days:             0,
			Interval:         6 * time.Hour,
			BatchSize:        5000,
			MaxBatchesPerRun: 0, // 0 = drain fully per run
		},
		NNTP: NNTPConfig{
			Port:                    563,
			TLS:                     true,
			MaxConns:                10,
			ConnectTimeout:          30 * time.Second,
			CircuitFailureThreshold: 3,
			CircuitCooldown:         30 * time.Second,
		},
		Scan: ScanConfig{
			BatchSize:           10000,
			Concurrency:         0, // 0 = derive from the NNTP pool size at startup
			Interval:            15 * time.Minute,
			DownstreamInterval:  5 * time.Minute,
			BuildInterval:       2 * time.Minute,
			PostProcessInterval: 5 * time.Minute,
			ForwardMaxArticles:  1000000,
			BackfillDays:        0,
			BackfillMaxArticles: 0,
		},
		Auth: AuthConfig{
			SessionTTL:       24 * time.Hour,
			DefaultRateLimit: 100,
			RateLimitWindow:  time.Hour,
		},
		LogLevel: "info",
	}
}

// DSNString returns a libpq connection string for the database config,
// preferring an explicit DSN when provided.
func (d DatabaseConfig) DSNString() string {
	if strings.TrimSpace(d.DSN) != "" {
		return d.DSN
	}
	return fmt.Sprintf(
		"host=%s port=%d user=%s password=%s dbname=%s sslmode=%s",
		d.Host, d.Port, d.User, d.Password, d.Name, d.SSLMode,
	)
}

// Load builds a Config from defaults, then an optional YAML file at path (when
// non-empty), then environment variables (which take highest precedence), and
// finally validates the result.
func Load(path string) (Config, error) {
	cfg := Default()

	if strings.TrimSpace(path) != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return Config{}, fmt.Errorf("read config file %q: %w", path, err)
		}
		if err := unmarshalYAML(data, &cfg); err != nil {
			return Config{}, fmt.Errorf("parse config file %q: %w", path, err)
		}
	}

	applyEnv(&cfg)

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// applyEnv overlays GOINDEX_-prefixed environment variables onto cfg.
func applyEnv(cfg *Config) {
	envStr("GOINDEX_LOG_LEVEL", &cfg.LogLevel)

	envStr("GOINDEX_SERVER_LISTEN_ADDR", &cfg.Server.ListenAddr)
	envStr("GOINDEX_SERVER_BASE_URL", &cfg.Server.BaseURL)
	envDur("GOINDEX_SERVER_READ_TIMEOUT", &cfg.Server.ReadTimeout)
	envDur("GOINDEX_SERVER_WRITE_TIMEOUT", &cfg.Server.WriteTimeout)

	envStr("GOINDEX_DB_DSN", &cfg.Database.DSN)
	envStr("GOINDEX_DB_HOST", &cfg.Database.Host)
	envInt("GOINDEX_DB_PORT", &cfg.Database.Port)
	envStr("GOINDEX_DB_USER", &cfg.Database.User)
	envStr("GOINDEX_DB_PASSWORD", &cfg.Database.Password)
	envStr("GOINDEX_DB_NAME", &cfg.Database.Name)
	envStr("GOINDEX_DB_SSL_MODE", &cfg.Database.SSLMode)
	envInt("GOINDEX_DB_MAX_CONNS", &cfg.Database.MaxConns)
	envInt("GOINDEX_DB_RESERVED_CONNS", &cfg.Database.ReservedConns)

	envStr("GOINDEX_NNTP_HOST", &cfg.NNTP.Host)
	envInt("GOINDEX_NNTP_PORT", &cfg.NNTP.Port)
	envBool("GOINDEX_NNTP_TLS", &cfg.NNTP.TLS)
	envStr("GOINDEX_NNTP_USERNAME", &cfg.NNTP.Username)
	envStr("GOINDEX_NNTP_PASSWORD", &cfg.NNTP.Password)
	envInt("GOINDEX_NNTP_MAX_CONNS", &cfg.NNTP.MaxConns)
	envDur("GOINDEX_NNTP_CONNECT_TIMEOUT", &cfg.NNTP.ConnectTimeout)
	envInt("GOINDEX_NNTP_CIRCUIT_FAILURE_THRESHOLD", &cfg.NNTP.CircuitFailureThreshold)
	envDur("GOINDEX_NNTP_CIRCUIT_COOLDOWN", &cfg.NNTP.CircuitCooldown)

	envStrSlice("GOINDEX_SCAN_GROUPS", &cfg.Scan.Groups)
	envInt("GOINDEX_SCAN_BATCH_SIZE", &cfg.Scan.BatchSize)
	envInt("GOINDEX_SCAN_CONCURRENCY", &cfg.Scan.Concurrency)
	envDur("GOINDEX_SCAN_INTERVAL", &cfg.Scan.Interval)
	envDur("GOINDEX_SCAN_DOWNSTREAM_INTERVAL", &cfg.Scan.DownstreamInterval)
	envDur("GOINDEX_SCAN_BUILD_INTERVAL", &cfg.Scan.BuildInterval)
	envDur("GOINDEX_SCAN_POSTPROCESS_INTERVAL", &cfg.Scan.PostProcessInterval)
	envInt("GOINDEX_SCAN_FORWARD_MAX_ARTICLES", &cfg.Scan.ForwardMaxArticles)
	envInt("GOINDEX_SCAN_BACKFILL_DAYS", &cfg.Scan.BackfillDays)
	envInt("GOINDEX_SCAN_BACKFILL_MAX_ARTICLES", &cfg.Scan.BackfillMaxArticles)

	envBool("GOINDEX_METADATA_ENABLED", &cfg.Metadata.Enabled)
	envStr("GOINDEX_METADATA_PROVIDER", &cfg.Metadata.Provider)
	envDur("GOINDEX_METADATA_INTERVAL", &cfg.Metadata.Interval)

	envBool("GOINDEX_RETENTION_ENABLED", &cfg.Retention.Enabled)
	envInt("GOINDEX_RETENTION_DAYS", &cfg.Retention.Days)
	envDur("GOINDEX_RETENTION_INTERVAL", &cfg.Retention.Interval)
	envInt("GOINDEX_RETENTION_BATCH_SIZE", &cfg.Retention.BatchSize)
	envInt("GOINDEX_RETENTION_MAX_BATCHES_PER_RUN", &cfg.Retention.MaxBatchesPerRun)

	envStr("GOINDEX_AUTH_JWT_SECRET", &cfg.Auth.JWTSecret)
	envDur("GOINDEX_AUTH_SESSION_TTL", &cfg.Auth.SessionTTL)
	envInt("GOINDEX_AUTH_DEFAULT_RATE_LIMIT", &cfg.Auth.DefaultRateLimit)
	envDur("GOINDEX_AUTH_RATE_LIMIT_WINDOW", &cfg.Auth.RateLimitWindow)
}

// Validate checks that required fields are present and values are sane.
func (c Config) Validate() error {
	var errs []string

	if c.Server.ListenAddr == "" {
		errs = append(errs, "server.listen_addr is required")
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		errs = append(errs, fmt.Sprintf("log_level %q is invalid (want debug|info|warn|error)", c.LogLevel))
	}

	if c.Database.DSN == "" {
		if c.Database.Host == "" {
			errs = append(errs, "database.host is required (or set database.dsn)")
		}
		if c.Database.Port <= 0 || c.Database.Port > 65535 {
			errs = append(errs, "database.port must be between 1 and 65535")
		}
		if c.Database.Name == "" {
			errs = append(errs, "database.name is required (or set database.dsn)")
		}
	}
	if c.Database.ReservedConns >= c.Database.MaxConns {
		errs = append(errs, "database.reserved_conns must be less than database.max_conns (leave at least one connection for the pipeline)")
	}
	if c.Database.MaxConns <= 0 {
		errs = append(errs, "database.max_conns must be greater than 0")
	}

	if c.NNTP.Host == "" {
		errs = append(errs, "nntp.host is required")
	}
	if c.NNTP.Port <= 0 || c.NNTP.Port > 65535 {
		errs = append(errs, "nntp.port must be between 1 and 65535")
	}
	if c.NNTP.MaxConns <= 0 {
		errs = append(errs, "nntp.max_conns must be greater than 0")
	}

	if c.Scan.BatchSize <= 0 {
		errs = append(errs, "scan.batch_size must be greater than 0")
	}
	if c.Scan.Concurrency < 0 {
		errs = append(errs, "scan.concurrency must not be negative (0 = auto)")
	}
	if c.Scan.Interval <= 0 {
		errs = append(errs, "scan.interval must be greater than 0")
	}
	if c.Scan.BackfillDays < 0 {
		errs = append(errs, "scan.backfill_days must not be negative")
	}
	if c.Scan.BackfillMaxArticles < 0 {
		errs = append(errs, "scan.backfill_max_articles must not be negative")
	}

	if c.Retention.Days < 0 {
		errs = append(errs, "retention.days must not be negative")
	}
	if c.Retention.Enabled && c.Retention.Days <= 0 {
		errs = append(errs, "retention.days must be greater than 0 when retention.enabled is true")
	}
	if c.Retention.BatchSize < 0 {
		errs = append(errs, "retention.batch_size must not be negative (0 = default)")
	}
	if c.Retention.MaxBatchesPerRun < 0 {
		errs = append(errs, "retention.max_batches_per_run must not be negative (0 = drain fully)")
	}

	if c.Auth.DefaultRateLimit <= 0 {
		errs = append(errs, "auth.default_rate_limit must be greater than 0")
	}
	if c.Auth.RateLimitWindow <= 0 {
		errs = append(errs, "auth.rate_limit_window must be greater than 0")
	}

	if len(errs) > 0 {
		return fmt.Errorf("invalid configuration:\n  - %s", strings.Join(errs, "\n  - "))
	}
	return nil
}

// Summary returns a human-readable, secret-redacted description of the config
// suitable for logging at startup.
func (c Config) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "log_level=%s\n", c.LogLevel)
	fmt.Fprintf(&b, "server: listen=%s base_url=%s read_timeout=%s write_timeout=%s\n",
		c.Server.ListenAddr, orNone(c.Server.BaseURL), c.Server.ReadTimeout, c.Server.WriteTimeout)
	if c.Database.DSN != "" {
		fmt.Fprintf(&b, "database: dsn=%s max_conns=%d\n", redact(c.Database.DSN), c.Database.MaxConns)
	} else {
		fmt.Fprintf(&b, "database: host=%s port=%d user=%s password=%s name=%s ssl_mode=%s max_conns=%d\n",
			c.Database.Host, c.Database.Port, c.Database.User, redact(c.Database.Password),
			c.Database.Name, c.Database.SSLMode, c.Database.MaxConns)
	}
	fmt.Fprintf(&b, "nntp: host=%s port=%d tls=%t username=%s password=%s max_conns=%d connect_timeout=%s\n",
		c.NNTP.Host, c.NNTP.Port, c.NNTP.TLS, orNone(c.NNTP.Username), redact(c.NNTP.Password),
		c.NNTP.MaxConns, c.NNTP.ConnectTimeout)
	fmt.Fprintf(&b, "scan: groups=%d batch_size=%d interval=%s backfill_days=%d backfill_max_articles=%d\n",
		len(c.Scan.Groups), c.Scan.BatchSize, c.Scan.Interval, c.Scan.BackfillDays, c.Scan.BackfillMaxArticles)
	fmt.Fprintf(&b, "auth: jwt_secret=%s session_ttl=%s default_rate_limit=%d rate_limit_window=%s",
		redact(c.Auth.JWTSecret), c.Auth.SessionTTL, c.Auth.DefaultRateLimit, c.Auth.RateLimitWindow)
	return b.String()
}

// redact masks a secret value, revealing only whether it is set.
func redact(s string) string {
	if s == "" {
		return "(unset)"
	}
	return "***"
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

// --- environment variable helpers ---

func envStr(key string, target *string) {
	if v, ok := os.LookupEnv(key); ok {
		*target = v
	}
}

func envInt(key string, target *int) {
	if v, ok := os.LookupEnv(key); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			*target = n
		}
	}
}

func envBool(key string, target *bool) {
	if v, ok := os.LookupEnv(key); ok {
		if b, err := strconv.ParseBool(strings.TrimSpace(v)); err == nil {
			*target = b
		}
	}
}

func envDur(key string, target *time.Duration) {
	if v, ok := os.LookupEnv(key); ok {
		if d, err := time.ParseDuration(strings.TrimSpace(v)); err == nil {
			*target = d
		}
	}
}

// envStrSlice reads a comma-separated list, trimming whitespace and dropping
// empty entries.
func envStrSlice(key string, target *[]string) {
	if v, ok := os.LookupEnv(key); ok {
		var out []string
		for _, part := range strings.Split(v, ",") {
			if p := strings.TrimSpace(part); p != "" {
				out = append(out, p)
			}
		}
		*target = out
	}
}
