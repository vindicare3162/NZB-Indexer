// Package metadata matches a release to an external title (TV show / movie) and
// returns descriptive metadata (title, year, season/episode, cover art,
// overview). Providers are pluggable behind the Provider interface; a keyless
// TVMaze TV provider is included. The feature is optional: when disabled or a
// provider errors, callers degrade gracefully and releases simply carry no
// metadata.
package metadata

import "context"

// Query describes what to look up, parsed from a release name.
type Query struct {
	// Title is the cleaned show/movie title (without season/episode/quality
	// tags), e.g. "the expanse".
	Title string
	// Year is the release year when present (0 when unknown). Helps disambiguate
	// movies and show reboots.
	Year int
	// Season / Episode are set for TV queries (0 when not applicable).
	Season  int
	Episode int
	// IsTV indicates the release was categorized as TV; movie-only providers can
	// skip non-TV queries and vice versa.
	IsTV bool
}

// Result is the metadata a provider returned for a Query.
type Result struct {
	Title      string
	Year       int
	Season     int
	Episode    int
	Source     string // provider name, e.g. "tvmaze"
	ExternalID string // provider's id for the matched title
	PosterURL  string
	Overview   string
	// Identifiers are normalized external IDs the provider resolved for the
	// matched title, keyed by source ("imdb"/"tvdb"/"tmdb"). These are persisted
	// as release identifiers (#108) so integrations can match by provider id.
	// Values are the raw provider values; the store normalizes/validates them.
	Identifiers map[string]string
}

// Provider looks up metadata for a query. Lookup returns (result, true, nil) on
// a match, (_, false, nil) when nothing matched (a definitive miss), and an
// error only for transient/provider failures (which callers may retry).
type Provider interface {
	// Name identifies the provider (stored as the metadata source).
	Name() string
	// Lookup resolves a query to metadata.
	Lookup(ctx context.Context, q Query) (Result, bool, error)
}
