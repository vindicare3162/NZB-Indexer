package store

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

// ReleaseIdentifier is a normalized external identifier for a release (e.g. an
// IMDb, TVDB, or TMDB id).
type ReleaseIdentifier struct {
	Source     string `json:"source"`
	Identifier string `json:"identifier"`
}

// Supported identifier sources.
const (
	IDSourceIMDB = "imdb"
	IDSourceTVDB = "tvdb"
	IDSourceTMDB = "tmdb"
)

var (
	reDigits = regexp.MustCompile(`^\d+$`)
	reIMDB   = regexp.MustCompile(`^tt\d{6,}$`)
)

// NormalizeIdentifier canonicalises a (source, identifier) pair per the
// source's rules and reports whether it is valid. Unknown sources are rejected
// so we never advertise/accept identifiers we cannot match on.
//
//   - imdb: digits, optionally "tt"-prefixed -> canonical "tt<digits>" (>=6 digits).
//   - tvdb, tmdb: decimal digits only, returned as-is.
func NormalizeIdentifier(source, identifier string) (ReleaseIdentifier, bool) {
	src := strings.ToLower(strings.TrimSpace(source))
	val := strings.TrimSpace(identifier)
	switch src {
	case IDSourceIMDB:
		v := strings.ToLower(val)
		v = strings.TrimPrefix(v, "tt")
		if !reDigits.MatchString(v) {
			return ReleaseIdentifier{}, false
		}
		// Pad to at least 7 digits (IMDb canonical minimum) is not required, but
		// enforce a sane minimum length so junk is rejected.
		for len(v) < 7 {
			v = "0" + v
		}
		id := ReleaseIdentifier{Source: IDSourceIMDB, Identifier: "tt" + v}
		if !reIMDB.MatchString(id.Identifier) {
			return ReleaseIdentifier{}, false
		}
		return id, true
	case IDSourceTVDB, IDSourceTMDB:
		if !reDigits.MatchString(val) {
			return ReleaseIdentifier{}, false
		}
		return ReleaseIdentifier{Source: src, Identifier: val}, true
	default:
		return ReleaseIdentifier{}, false
	}
}

// SupportedIDSources returns the identifier sources the system can match on.
func SupportedIDSources() []string {
	return []string{IDSourceIMDB, IDSourceTVDB, IDSourceTMDB}
}

// AddReleaseIdentifier normalizes and stores an identifier for a release. It is
// idempotent: a duplicate (release, source, identifier) is a no-op. Invalid
// identifiers return an error and store nothing.
func (s *Store) AddReleaseIdentifier(ctx context.Context, releaseID int64, source, identifier string) error {
	norm, ok := NormalizeIdentifier(source, identifier)
	if !ok {
		return fmt.Errorf("invalid identifier %q for source %q", identifier, source)
	}
	_, err := s.pool.Exec(ctx, `
INSERT INTO release_identifiers (release_id, source, identifier)
VALUES ($1, $2, $3)
ON CONFLICT (release_id, source, identifier) DO NOTHING`,
		releaseID, norm.Source, norm.Identifier)
	if err != nil {
		return fmt.Errorf("add release identifier: %w", err)
	}
	return nil
}

// GetReleaseIdentifiers returns the identifiers stored for a release, ordered
// by source.
func (s *Store) GetReleaseIdentifiers(ctx context.Context, releaseID int64) ([]ReleaseIdentifier, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT source, identifier FROM release_identifiers WHERE release_id = $1 ORDER BY source, identifier`,
		releaseID)
	if err != nil {
		return nil, fmt.Errorf("get release identifiers: %w", err)
	}
	defer rows.Close()

	var out []ReleaseIdentifier
	for rows.Next() {
		var id ReleaseIdentifier
		if err := rows.Scan(&id.Source, &id.Identifier); err != nil {
			return nil, fmt.Errorf("scan identifier: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
