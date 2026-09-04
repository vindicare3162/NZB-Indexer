package store

import (
	"context"
	"encoding/json"
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
	// Obfuscated marks a release whose name looks like random hex/base64 (no
	// real words); such releases are excluded from default search results.
	Obfuscated bool
}

// CreateRelease inserts a release. When a release with the same release_hash
// already exists, no row is inserted and created=false is returned along with
// the existing release. This provides deduplication.
func (s *Store) CreateRelease(ctx context.Context, in ReleaseInput) (Release, bool, error) {
	const ins = `
INSERT INTO releases
    (guid, name, original_subject, search_name, category_id, group_id, binary_id, poster,
     total_parts, size_bytes, posted_at, release_hash, pp_status, obfuscated)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,'pending',$13)
ON CONFLICT (release_hash) DO NOTHING
RETURNING id, guid, name, original_subject, search_name, category_id, group_id, binary_id,
          poster, total_parts, size_bytes, posted_at, release_hash, pp_status,
          nfo, grabs, created_at, updated_at`

	var r Release
	err := s.pool.QueryRow(ctx, ins,
		in.GUID, in.Name, in.OriginalSubject, in.SearchName, in.CategoryID,
		in.GroupID, in.BinaryID, in.Poster, in.TotalParts, in.SizeBytes, in.PostedAt, in.ReleaseHash,
		in.Obfuscated,
	).Scan(&r.ID, &r.GUID, &r.Name, &r.OriginalSubject, &r.SearchName,
		&r.CategoryID, &r.GroupID, &r.BinaryID, &r.Poster, &r.TotalParts, &r.SizeBytes,
		&r.PostedAt, &r.ReleaseHash, &r.PPStatus, &r.NFO, &r.Grabs,
		&r.CreatedAt, &r.UpdatedAt)

	if err == nil {
		// Snapshot the binary's ordered segments into durable release storage so
		// this release can generate its NZB after the raw parts are pruned. A
		// failure here is non-fatal to release creation (the parts join remains a
		// fallback), so log-and-continue semantics apply at the caller; we return
		// the error only when it is a real DB fault.
		if serr := s.snapshotReleaseSegments(ctx, r.ID); serr != nil {
			return r, true, fmt.Errorf("snapshot segments for release %d: %w", r.ID, serr)
		}
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

// jsonSegment is the persisted form of a segment in releases.segments.
type jsonSegment struct {
	MessageID  string `json:"message_id"`
	Bytes      int64  `json:"bytes"`
	PartNumber int    `json:"number"`
	Subject    string `json:"subject"`
}

// GetReleaseSegments returns the ordered part segments backing a release. It
// prefers the durable, denormalized segments stored on the release
// (releases.segments), so NZB generation works after the raw parts have been
// pruned. When a (legacy) release has no durable segments, it falls back to
// resolving them from the raw `parts` via the release's binary_id. Returns an
// empty slice when neither source has segments.
func (s *Store) GetReleaseSegments(ctx context.Context, releaseID int64) ([]PartSegment, error) {
	// 1. Durable segments on the release.
	var raw []byte
	if err := s.pool.QueryRow(ctx,
		`SELECT segments FROM releases WHERE id = $1`, releaseID).Scan(&raw); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("read release segments: %w", err)
	}
	if len(raw) > 0 {
		var js []jsonSegment
		if err := json.Unmarshal(raw, &js); err != nil {
			return nil, fmt.Errorf("unmarshal release segments: %w", err)
		}
		if len(js) > 0 {
			out := make([]PartSegment, len(js))
			for i, j := range js {
				out[i] = PartSegment{MessageID: j.MessageID, Bytes: j.Bytes, PartNumber: j.PartNumber, Subject: j.Subject}
			}
			return out, nil
		}
	}

	// 2. Fallback: resolve from raw parts (legacy releases not yet snapshotted).
	return s.releaseSegmentsFromParts(ctx, releaseID)
}

// releaseSegmentsFromParts resolves a release's ordered segments from the raw
// parts table via binary_id (the pre-durable-segments behaviour).
func (s *Store) releaseSegmentsFromParts(ctx context.Context, releaseID int64) ([]PartSegment, error) {
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

// snapshotReleaseSegments captures the release's ordered segments (from its
// linked binary's parts) into durable releases.segments storage. A release with
// no linked binary or no parts stores an empty array. Idempotent: safe to call
// again to refresh.
func (s *Store) snapshotReleaseSegments(ctx context.Context, releaseID int64) error {
	segs, err := s.releaseSegmentsFromParts(ctx, releaseID)
	if err != nil {
		return err
	}
	js := make([]jsonSegment, len(segs))
	for i, seg := range segs {
		js[i] = jsonSegment{MessageID: seg.MessageID, Bytes: seg.Bytes, PartNumber: seg.PartNumber, Subject: seg.Subject}
	}
	data, err := json.Marshal(js)
	if err != nil {
		return fmt.Errorf("marshal segments: %w", err)
	}
	if _, err := s.pool.Exec(ctx,
		`UPDATE releases SET segments = $2, updated_at = now() WHERE id = $1`, releaseID, data); err != nil {
		return fmt.Errorf("store release segments: %w", err)
	}
	return nil
}

// BackfillReleaseSegments snapshots durable segments for legacy releases that
// have none yet (empty segments array) but still have a linked binary with
// parts. It processes up to limit releases and returns how many were repaired
// and how many could not be (no resolvable parts). Used by the retention
// prerequisite to make existing installs retention-safe.
func (s *Store) BackfillReleaseSegments(ctx context.Context, limit int) (repaired, unresolved int, err error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := s.pool.Query(ctx, `
SELECT id FROM releases
WHERE segments = '[]'::jsonb AND binary_id IS NOT NULL
ORDER BY id
LIMIT $1`, limit)
	if err != nil {
		return 0, 0, fmt.Errorf("list releases needing segment backfill: %w", err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, 0, fmt.Errorf("scan release id: %w", err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}

	for _, id := range ids {
		segs, serr := s.releaseSegmentsFromParts(ctx, id)
		if serr != nil {
			return repaired, unresolved, serr
		}
		if len(segs) == 0 {
			unresolved++ // parts already gone / never present; cannot repair
			continue
		}
		if serr := s.snapshotReleaseSegments(ctx, id); serr != nil {
			return repaired, unresolved, serr
		}
		repaired++
	}
	return repaired, unresolved, nil
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
	// IncludeObfuscated includes releases whose name is still obfuscated. By
	// default (false) these unusable releases are excluded from results.
	IncludeObfuscated bool
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

	// Tokenise the query and require every token to appear somewhere in the
	// search_name (order-independent AND). A single substring match would fail
	// for multi-word / tvsearch queries whose words aren't adjacent in the
	// release name (e.g. "saving s03e10" vs "saving grace s03e10 hdtv xvid").
	if q := trimLower(f.Query); q != "" {
		for _, tok := range strings.Fields(q) {
			args = append(args, "%"+tok+"%")
			clauses = append(clauses, fmt.Sprintf("search_name LIKE $%d", len(args)))
		}
	}

	if !f.IncludeObfuscated {
		clauses = append(clauses, "obfuscated = false")
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
