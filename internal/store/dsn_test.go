package store

import (
	"strings"
	"testing"
)

func TestEnsurePgx5Scheme(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string // exact match, or checked via contains below
	}{
		{"postgres url", "postgres://u:p@h:5432/db?sslmode=disable", "pgx5://u:p@h:5432/db?sslmode=disable"},
		{"postgresql url", "postgresql://u:p@h:5432/db", "pgx5://u:p@h:5432/db"},
		{"already pgx5", "pgx5://u:p@h:5432/db", "pgx5://u:p@h:5432/db"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ensurePgx5Scheme(c.in); got != c.want {
				t.Errorf("ensurePgx5Scheme(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestKeyValueDSNToURL(t *testing.T) {
	// This is the form DatabaseConfig.DSNString() produces from discrete
	// fields (the Docker/compose path that previously broke migrations).
	dsn := "host=db port=5432 user=goindex password=secret dbname=goindex sslmode=disable"
	got := ensurePgx5Scheme(dsn)

	if !strings.HasPrefix(got, "pgx5://") {
		t.Fatalf("expected pgx5:// scheme, got %q", got)
	}
	for _, want := range []string{"goindex:secret@", "db:5432", "/goindex", "sslmode=disable"} {
		if !strings.Contains(got, want) {
			t.Errorf("URL %q missing %q", got, want)
		}
	}
}

func TestKeyValueDSNDefaults(t *testing.T) {
	// Missing host/port fall back to localhost:5432.
	got := ensurePgx5Scheme("user=x dbname=y")
	if !strings.Contains(got, "localhost:5432") {
		t.Errorf("expected localhost:5432 defaults, got %q", got)
	}
}
