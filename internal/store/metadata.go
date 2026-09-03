package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// ReleaseMetadataInput is the data written for a release's metadata. A miss
// (matched=false) is still recorded so the enrichment loop does not retry a
// permanent non-match every pass.
type ReleaseMetadataInput struct {
	ReleaseID  int64
	Title      string
	Year       *int
	Season     *int
	Episode    *int
	Source     string
	ExternalID string
	PosterURL  string
	Overview   string
	Matched    bool
}

// UpsertReleaseMetadata inserts or replaces a release's metadata row.
func (s *Store) UpsertReleaseMetadata(ctx context.Context, in ReleaseMetadataInput) error {
	const q = `
INSERT INTO release_metadata
    (release_id, title, year, season, episode, source, external_id, poster_url, overview, matched, fetched_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10, now())
ON CONFLICT (release_id) DO UPDATE SET
    title = EXCLUDED.title, year = EXCLUDED.year, season = EXCLUDED.season,
    episode = EXCLUDED.episode, source = EXCLUDED.source,
    external_id = EXCLUDED.external_id, poster_url = EXCLUDED.poster_url,
    overview = EXCLUDED.overview, matched = EXCLUDED.matched, fetched_at = now()`
	_, err := s.pool.Exec(ctx, q,
		in.ReleaseID, in.Title, in.Year, in.Season, in.Episode, in.Source,
		in.ExternalID, in.PosterURL, in.Overview, in.Matched)
	if err != nil {
		return fmt.Errorf("upsert release metadata: %w", err)
	}
	return nil
}

// GetReleaseMetadata returns a release's metadata, or ErrNotFound when the
// release has not been enriched.
func (s *Store) GetReleaseMetadata(ctx context.Context, releaseID int64) (ReleaseMetadata, error) {
	const q = `
SELECT release_id, title, year, season, episode, source, external_id, poster_url, overview, matched, fetched_at
FROM release_metadata WHERE release_id = $1`
	var m ReleaseMetadata
	err := s.pool.QueryRow(ctx, q, releaseID).Scan(
		&m.ReleaseID, &m.Title, &m.Year, &m.Season, &m.Episode, &m.Source,
		&m.ExternalID, &m.PosterURL, &m.Overview, &m.Matched, &m.FetchedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ReleaseMetadata{}, ErrNotFound
	}
	if err != nil {
		return ReleaseMetadata{}, fmt.Errorf("get release metadata: %w", err)
	}
	return m, nil
}

// ReleaseForEnrichment is the minimal release view the enrichment loop needs.
type ReleaseForEnrichment struct {
	ID         int64
	Name       string
	CategoryID *int
}

// ListReleasesNeedingMetadata returns releases that have no metadata row yet,
// limited to TV categories (5000–5999) since the keyless default provider only
// resolves TV. Newest first, bounded by limit.
func (s *Store) ListReleasesNeedingMetadata(ctx context.Context, limit int) ([]ReleaseForEnrichment, error) {
	if limit <= 0 {
		limit = 100
	}
	const q = `
SELECT r.id, r.name, r.category_id
FROM releases r
LEFT JOIN release_metadata m ON m.release_id = r.id
WHERE m.release_id IS NULL
  AND r.category_id BETWEEN 5000 AND 5999
  AND r.obfuscated = FALSE
ORDER BY coalesce(r.posted_at, r.created_at) DESC, r.id DESC
LIMIT $1`
	rows, err := s.pool.Query(ctx, q, limit)
	if err != nil {
		return nil, fmt.Errorf("list releases needing metadata: %w", err)
	}
	defer rows.Close()

	var out []ReleaseForEnrichment
	for rows.Next() {
		var r ReleaseForEnrichment
		if err := rows.Scan(&r.ID, &r.Name, &r.CategoryID); err != nil {
			return nil, fmt.Errorf("scan release for enrichment: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
