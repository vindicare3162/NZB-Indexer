package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// trimLower trims and lowercases a search query for case-insensitive LIKE.
func trimLower(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// ReleaseInput is the data needed to create a release.
type ReleaseInput struct {
	GUID            string
	Name            string
	OriginalSubject string
	SearchName      string
	CategoryID      *int
	GroupID         *int64
	BinaryID        *int64
	Poster          string
	TotalParts      int
	SizeBytes       int64
	PostedAt        *time.Time
	ReleaseHash     string
}

// CreateRelease inserts a release. When a release with the same release_hash
// already exists, no row is inserted and created=false is returned along with
// the existing release. This provides deduplication.
func (s *Store) CreateRelease(ctx context.Context, in ReleaseInput) (Release, bool, error) {
	const ins = `
INSERT INTO releases
    (guid, name, original_subject, search_name, category_id, group_id, binary_id, poster,
     total_parts, size_bytes, posted_at, release_hash, pp_status)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,'pending')
ON CONFLICT (release_hash) DO NOTHING
RETURNING id, guid, name, original_subject, search_name, category_id, group_id, binary_id,
          poster, total_parts, size_bytes, posted_at, release_hash, pp_status,
          nfo, grabs, created_at, updated_at`

	var r Release
	err := s.pool.QueryRow(ctx, ins,
		in.GUID, in.Name, in.OriginalSubject, in.SearchName, in.CategoryID,
		in.GroupID, in.BinaryID, in.Poster, in.TotalParts, in.SizeBytes, in.PostedAt, in.ReleaseHash,
	).Scan(&r.ID, &r.GUID, &r.Name, &r.OriginalSubject, &r.SearchName,
		&r.CategoryID, &r.GroupID, &r.BinaryID, &r.Poster, &r.TotalParts, &r.SizeBytes,
		&r.PostedAt, &r.ReleaseHash, &r.PPStatus, &r.NFO, &r.Grabs,
		&r.CreatedAt, &r.UpdatedAt)

	if err == nil {
		return r, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Release{}, false, fmt.Errorf("insert release: %w", err)
	}

	// Conflict: fetch and return the existing release.
	existing, gerr := s.GetReleaseByHash(ctx, in.ReleaseHash)
	if gerr != nil {
		return Release{}, false, fmt.Errorf("lookup existing release: %w", gerr)
	}
	return existing, false, nil
}

// GetReleaseByHash returns the release with the given hash, or ErrNotFound.
func (s *Store) GetReleaseByHash(ctx context.Context, hash string) (Release, error) {
	return s.scanRelease(ctx, `WHERE release_hash = $1`, hash)
}

// GetReleaseByGUID returns the release with the given GUID, or ErrNotFound.
func (s *Store) GetReleaseByGUID(ctx context.Context, guid string) (Release, error) {
	return s.scanRelease(ctx, `WHERE guid = $1`, guid)
}

// scanRelease runs a single-row release query with the given WHERE clause.
func (s *Store) scanRelease(ctx context.Context, where string, args ...any) (Release, error) {
	q := `
SELECT id, guid, name, original_subject, search_name, category_id, group_id, binary_id,
       poster, total_parts, size_bytes, posted_at, release_hash, pp_status,
       nfo, grabs, created_at, updated_at
FROM releases ` + where

	var r Release
	err := s.pool.QueryRow(ctx, q, args...).Scan(
		&r.ID, &r.GUID, &r.Name, &r.OriginalSubject, &r.SearchName,
		&r.CategoryID, &r.GroupID, &r.BinaryID, &r.Poster, &r.TotalParts, &r.SizeBytes,
		&r.PostedAt, &r.ReleaseHash, &r.PPStatus, &r.NFO, &r.Grabs,
		&r.CreatedAt, &r.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Release{}, ErrNotFound
	}
	if err != nil {
		return Release{}, fmt.Errorf("scan release: %w", err)
	}
	return r, nil
}

// PartSegment is a single article segment used to build an NZB file entry.
type PartSegment struct {
	MessageID  string
	Bytes      int64
	PartNumber int
	Subject    string
}

// GetReleaseSegments returns the ordered part segments backing a release,
// resolved via the release's binary_id. Segments are ordered by part number so
// the generated NZB reconstructs the file correctly. Returns an empty slice
// when the release has no linked binary.
func (s *Store) GetReleaseSegments(ctx context.Context, releaseID int64) ([]PartSegment, error) {
	const q = `
SELECT p.message_id, p.bytes, p.part_number, p.subject
FROM parts p
JOIN releases r ON r.binary_id = p.binary_id
WHERE r.id = $1 AND p.message_id <> ''
ORDER BY p.part_number, p.article_number`

	rows, err := s.pool.Query(ctx, q, releaseID)
	if err != nil {
		return nil, fmt.Errorf("get release segments: %w", err)
	}
	defer rows.Close()

	var out []PartSegment
	for rows.Next() {
		var seg PartSegment
		if err := rows.Scan(&seg.MessageID, &seg.Bytes, &seg.PartNumber, &seg.Subject); err != nil {
			return nil, fmt.Errorf("scan segment: %w", err)
		}
		out = append(out, seg)
	}
	return out, rows.Err()
}

// SearchFilter parameterises a release search.
type SearchFilter struct {
	// Query is a free-text search over the release search_name. Empty matches
	// all.
	Query string
	// Categories restricts results to these Newznab category IDs (a parent id
	// like 2000 also matches its children 20xx). Empty matches all.
	Categories []int
	// Limit bounds the number of rows returned.
	Limit int
	// Offset is the pagination offset.
	Offset int
}

// SearchReleases returns releases matching the filter (newest first) plus the
// total count of matches (ignoring limit/offset) for pagination.
func (s *Store) SearchReleases(ctx context.Context, f SearchFilter) ([]Release, int, error) {
	where, args := buildSearchWhere(f)

	// Total count for pagination.
	var total int
	countQ := `SELECT count(*) FROM releases` + where
	if err := s.pool.QueryRow(ctx, countQ, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count search: %w", err)
	}

	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}
	args = append(args, limit, f.Offset)
	listQ := fmt.Sprintf(`
SELECT id, guid, name, original_subject, search_name, category_id, group_id, binary_id,
       poster, total_parts, size_bytes, posted_at, release_hash, pp_status,
       nfo, grabs, created_at, updated_at
FROM releases%s
ORDER BY coalesce(posted_at, created_at) DESC, id DESC
LIMIT $%d OFFSET $%d`, where, len(args)-1, len(args))

	rows, err := s.pool.Query(ctx, listQ, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("search releases: %w", err)
	}
	defer rows.Close()

	var out []Release
	for rows.Next() {
		var r Release
		if err := rows.Scan(&r.ID, &r.GUID, &r.Name, &r.OriginalSubject, &r.SearchName,
			&r.CategoryID, &r.GroupID, &r.BinaryID, &r.Poster, &r.TotalParts, &r.SizeBytes,
			&r.PostedAt, &r.ReleaseHash, &r.PPStatus, &r.NFO, &r.Grabs,
			&r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, 0, fmt.Errorf("scan release: %w", err)
		}
		out = append(out, r)
	}
	return out, total, rows.Err()
}

// buildSearchWhere assembles the WHERE clause and positional args shared by the
// count and list queries.
func buildSearchWhere(f SearchFilter) (string, []any) {
	var clauses []string
	var args []any

	if q := trimLower(f.Query); q != "" {
		args = append(args, "%"+q+"%")
		clauses = append(clauses, fmt.Sprintf("search_name LIKE $%d", len(args)))
	}

	if len(f.Categories) > 0 {
		// Expand each category into an exact match plus, for parent categories
		// (multiples of 1000), the child range [cat, cat+999].
		var catClauses []string
		for _, c := range f.Categories {
			if c%1000 == 0 {
				args = append(args, c, c+999)
				catClauses = append(catClauses,
					fmt.Sprintf("(category_id BETWEEN $%d AND $%d)", len(args)-1, len(args)))
			} else {
				args = append(args, c)
				catClauses = append(catClauses, fmt.Sprintf("category_id = $%d", len(args)))
			}
		}
		clauses = append(clauses, "("+strings.Join(catClauses, " OR ")+")")
	}

	if len(clauses) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(clauses, " AND "), args
}

// IncrementGrabs bumps a release's grab counter (called when its NZB is
// downloaded).
func (s *Store) IncrementGrabs(ctx context.Context, id int64) error {
	_, err := s.pool.Exec(ctx, `UPDATE releases SET grabs = grabs + 1 WHERE id = $1`, id)
	return err
}

// CountReleases returns the total number of releases.
func (s *Store) CountReleases(ctx context.Context) (int64, error) {
	var n int64
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM releases`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count releases: %w", err)
	}
	return n, nil
}
