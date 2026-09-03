package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDefaultIsInvalidWithoutRequiredFields(t *testing.T) {
	cfg := Default()
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected default config to be invalid (missing nntp.host), got nil")
	}
}

func TestValidateReportsAllMissingRequired(t *testing.T) {
	cfg := Default()
	// Break several fields at once; Validate should aggregate them.
	cfg.NNTP.Host = ""
	cfg.Database.Host = ""
	cfg.Database.Name = ""
	cfg.LogLevel = "verbose"

	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error")
	}
	msg := err.Error()
	for _, want := range []string{"nntp.host", "database.host", "database.name", "log_level"} {
		if !strings.Contains(msg, want) {
			t.Errorf("expected error to mention %q, got:\n%s", want, msg)
		}
	}
}

func TestValidMinimalConfig(t *testing.T) {
	cfg := Default()
	cfg.NNTP.Host = "news.example.com"
	cfg.Database.Host = "localhost"
	cfg.Database.Name = "goindex"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected valid config, got: %v", err)
	}
}

func TestEnvOverridesDefaults(t *testing.T) {
	clearGoindexEnv(t)
	t.Setenv("GOINDEX_NNTP_HOST", "news.provider.net")
	t.Setenv("GOINDEX_NNTP_PORT", "119")
	t.Setenv("GOINDEX_NNTP_TLS", "false")
	t.Setenv("GOINDEX_NNTP_MAX_CONNS", "25")
	t.Setenv("GOINDEX_SCAN_GROUPS", "alt.binaries.foo, alt.binaries.bar ,")
	t.Setenv("GOINDEX_SCAN_INTERVAL", "5m")
	t.Setenv("GOINDEX_SCAN_DOWNSTREAM_INTERVAL", "3m")
	t.Setenv("GOINDEX_SCAN_BUILD_INTERVAL", "90s")
	t.Setenv("GOINDEX_SCAN_POSTPROCESS_INTERVAL", "7m")
	t.Setenv("GOINDEX_SCAN_FORWARD_MAX_ARTICLES", "250000")
	t.Setenv("GOINDEX_DB_NAME", "idx")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.NNTP.Host != "news.provider.net" {
		t.Errorf("NNTP.Host = %q, want news.provider.net", cfg.NNTP.Host)
	}
	if cfg.NNTP.Port != 119 {
		t.Errorf("NNTP.Port = %d, want 119", cfg.NNTP.Port)
	}
	if cfg.NNTP.TLS {
		t.Error("NNTP.TLS = true, want false")
	}
	if cfg.NNTP.MaxConns != 25 {
		t.Errorf("NNTP.MaxConns = %d, want 25", cfg.NNTP.MaxConns)
	}
	if len(cfg.Scan.Groups) != 2 || cfg.Scan.Groups[0] != "alt.binaries.foo" || cfg.Scan.Groups[1] != "alt.binaries.bar" {
		t.Errorf("Scan.Groups = %v, want [alt.binaries.foo alt.binaries.bar]", cfg.Scan.Groups)
	}
	if cfg.Scan.Interval != 5*time.Minute {
		t.Errorf("Scan.Interval = %s, want 5m", cfg.Scan.Interval)
	}
	if cfg.Scan.DownstreamInterval != 3*time.Minute {
		t.Errorf("Scan.DownstreamInterval = %s, want 3m", cfg.Scan.DownstreamInterval)
	}
	if cfg.Scan.BuildInterval != 90*time.Second {
		t.Errorf("Scan.BuildInterval = %s, want 90s", cfg.Scan.BuildInterval)
	}
	if cfg.Scan.PostProcessInterval != 7*time.Minute {
		t.Errorf("Scan.PostProcessInterval = %s, want 7m", cfg.Scan.PostProcessInterval)
	}
	if cfg.Scan.ForwardMaxArticles != 250000 {
		t.Errorf("Scan.ForwardMaxArticles = %d, want 250000", cfg.Scan.ForwardMaxArticles)
	}
}

// clearGoindexEnv unsets any ambient GOINDEX_* variables so a test's view of
// the environment is deterministic regardless of the surrounding shell.
func clearGoindexEnv(t *testing.T) {
	t.Helper()
	for _, kv := range os.Environ() {
		if i := strings.IndexByte(kv, '='); i > 0 {
			key := kv[:i]
			if strings.HasPrefix(key, "GOINDEX_") {
				t.Setenv(key, "") // restored by t.Cleanup
				os.Unsetenv(key)
			}
		}
	}
}

func TestLoadYAMLThenEnvPrecedence(t *testing.T) {
	clearGoindexEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yaml := `
log_level: debug
server:
  listen_addr: ":9000"
nntp:
  host: yaml.example.com
  port: 563
  max_conns: 8
database:
  host: db.example.com
  name: fromyaml
scan:
  groups:
    - alt.binaries.a
    - alt.binaries.b
  interval: 30m
`
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}

	// Env should override the YAML host.
	t.Setenv("GOINDEX_NNTP_HOST", "env.example.com")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("LogLevel = %q, want debug", cfg.LogLevel)
	}
	if cfg.Server.ListenAddr != ":9000" {
		t.Errorf("ListenAddr = %q, want :9000", cfg.Server.ListenAddr)
	}
	if cfg.NNTP.Host != "env.example.com" {
		t.Errorf("NNTP.Host = %q, want env.example.com (env should win)", cfg.NNTP.Host)
	}
	if cfg.Database.Name != "fromyaml" {
		t.Errorf("Database.Name = %q, want fromyaml", cfg.Database.Name)
	}
	if cfg.Scan.Interval != 30*time.Minute {
		t.Errorf("Scan.Interval = %s, want 30m", cfg.Scan.Interval)
	}
	if len(cfg.Scan.Groups) != 2 {
		t.Errorf("Scan.Groups = %v, want 2 entries", cfg.Scan.Groups)
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "does-not-exist.yaml")); err == nil {
		t.Fatal("expected error loading missing config file")
	}
}

func TestSummaryRedactsSecrets(t *testing.T) {
	cfg := Default()
	cfg.NNTP.Host = "news.example.com"
	cfg.NNTP.Password = "super-secret-pass"
	cfg.Database.Host = "localhost"
	cfg.Database.Name = "goindex"
	cfg.Database.Password = "db-secret"
	cfg.Auth.JWTSecret = "jwt-secret-value"

	s := cfg.Summary()
	for _, secret := range []string{"super-secret-pass", "db-secret", "jwt-secret-value"} {
		if strings.Contains(s, secret) {
			t.Errorf("summary leaked secret %q:\n%s", secret, s)
		}
	}
	if !strings.Contains(s, "***") {
		t.Errorf("summary should mask set secrets with ***:\n%s", s)
	}
}

func TestDSNString(t *testing.T) {
	d := DatabaseConfig{DSN: "postgres://u:p@h:5432/db"}
	if got := d.DSNString(); got != "postgres://u:p@h:5432/db" {
		t.Errorf("explicit DSN = %q", got)
	}

	d2 := DatabaseConfig{Host: "h", Port: 5432, User: "u", Password: "p", Name: "db", SSLMode: "disable"}
	got := d2.DSNString()
	for _, want := range []string{"host=h", "port=5432", "user=u", "password=p", "dbname=db", "sslmode=disable"} {
		if !strings.Contains(got, want) {
			t.Errorf("DSN %q missing %q", got, want)
		}
	}
}
