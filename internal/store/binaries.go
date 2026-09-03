package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// AssembleBinaries folds unassigned parts (binary_id IS NULL) into binaries,
// grouped by (group_id, norm_subject, poster). For each group it upserts a
// binary row (accumulating collected_parts, total_bytes, and the max declared
// total_parts and earliest posted_at), then links the parts to that binary.
//
// It processes at most `limit` groups per call to bound work, and returns the
// number of binaries touched. Empty norm_subject parts are ignored (they can't
// be grouped reliably).
func (s *Store) AssembleBinaries(ctx context.Context, limit int) (int, error) {
	if limit <= 0 {
		limit = 500
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// Pick candidate groupings from unassigned parts.
	const selectGroups = `
SELECT group_id, norm_subject, poster
FROM parts
WHERE binary_id IS NULL AND norm_subject <> ''
GROUP BY group_id, norm_subject, poster
ORDER BY max(created_at) DESC
LIMIT $1`

	rows, err := tx.Query(ctx, selectGroups, limit)
	if err != nil {
		return 0, fmt.Errorf("select groupings: %w", err)
	}

	type key struct {
		groupID     int64
		normSubject string
		poster      string
	}
	var keys []key
	for rows.Next() {
		var k key
		if err := rows.Scan(&k.groupID, &k.normSubject, &k.poster); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan grouping: %w", err)
		}
		keys = append(keys, k)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	touched := 0
	for _, k := range keys {
		if err := assembleOne(ctx, tx, k.groupID, k.normSubject, k.poster); err != nil {
			return 0, err
		}
		touched++
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit assemble: %w", err)
	}
	return touched, nil
}

// assembleOne upserts the binary for one grouping and links its parts. It runs
// inside the caller's transaction.
func assembleOne(ctx context.Context, tx pgx.Tx, groupID int64, normSubject, poster string) error {
	// Aggregate the unassigned parts for this grouping.
	const agg = `
SELECT
    count(*)                              AS collected,
    coalesce(max(total_parts), 0)         AS declared_total,
    coalesce(sum(bytes), 0)               AS total_bytes,
    min(posted_at)                        AS earliest
FROM parts
WHERE group_id = $1 AND norm_subject = $2 AND poster = $3 AND binary_id IS NULL`

	var (
		collected     int
		declaredTotal int
		totalBytes    int64
		earliest      *time.Time
	)
	if err := tx.QueryRow(ctx, agg, groupID, normSubject, poster).
		Scan(&collected, &declaredTotal, &totalBytes, &earliest); err != nil {
		return fmt.Errorf("aggregate parts: %w", err)
	}
	if collected == 0 {
		return nil
	}

	// Upsert the binary. On conflict we ADD the newly collected parts/bytes to
	// any existing binary (parts arriving across multiple scans), keep the
	// larger declared total, and recompute completeness.
	const upsert = `
INSERT INTO binaries
    (group_id, norm_subject, poster, total_parts, collected_parts, total_bytes, posted_at, complete)
VALUES ($1, $2, $3, $4::int, $5::int, $6::bigint, $7,
        ($5::int >= $4::int AND $4::int > 0))
ON CONFLICT (group_id, norm_subject, poster) DO UPDATE SET
    collected_parts = binaries.collected_parts + EXCLUDED.collected_parts,
    total_parts     = GREATEST(binaries.total_parts, EXCLUDED.total_parts),
    total_bytes     = binaries.total_bytes + EXCLUDED.total_bytes,
    posted_at       = LEAST(binaries.posted_at, EXCLUDED.posted_at),
    complete        = (
        (binaries.collected_parts + EXCLUDED.collected_parts)
            >= GREATEST(binaries.total_parts, EXCLUDED.total_parts)
        AND GREATEST(binaries.total_parts, EXCLUDED.total_parts) > 0
    ),
    updated_at      = now()
RETURNING id`

	var binaryID int64
	if err := tx.QueryRow(ctx, upsert,
		groupID, normSubject, poster, declaredTotal, collected, totalBytes, earliest,
	).Scan(&binaryID); err != nil {
		return fmt.Errorf("upsert binary: %w", err)
	}

	// Link the parts we just counted to this binary.
	const link = `
UPDATE parts SET binary_id = $4
WHERE group_id = $1 AND norm_subject = $2 AND poster = $3 AND binary_id IS NULL`
	if _, err := tx.Exec(ctx, link, groupID, normSubject, poster, binaryID); err != nil {
		return fmt.Errorf("link parts: %w", err)
	}
	return nil
}

// ListCompleteUnreleasedBinaries returns complete binaries not yet promoted to
// releases, up to limit.
func (s *Store) ListCompleteUnreleasedBinaries(ctx context.Context, limit int) ([]Binary, error) {
	if limit <= 0 {
		limit = 500
	}
	const q = `
SELECT id, group_id, norm_subject, poster, total_parts, collected_parts, total_bytes, posted_at, complete, released, created_at, updated_at
FROM binaries
WHERE complete = TRUE AND released = FALSE
ORDER BY updated_at
LIMIT $1`
	rows, err := s.pool.Query(ctx, q, limit)
	if err != nil {
		return nil, fmt.Errorf("list complete binaries: %w", err)
	}
	defer rows.Close()

	var out []Binary
	for rows.Next() {
		var b Binary
		if err := rows.Scan(&b.ID, &b.GroupID, &b.NormSubject, &b.Poster,
			&b.TotalParts, &b.CollectedParts, &b.TotalBytes, &b.PostedAt,
			&b.Complete, &b.Released, &b.CreatedAt, &b.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan binary: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// AgeOutStaleBinaries deletes incomplete, unreleased binaries whose most recent
// update is older than olderThan, on the assumption their missing parts will
// never arrive. The associated parts are deleted too, since an incomplete stale
// binary is unusable. Returns the number of binaries removed.
func (s *Store) AgeOutStaleBinaries(ctx context.Context, olderThan time.Duration) (int64, error) {
	cutoff := time.Now().Add(-olderThan)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// Remove parts belonging to the stale incomplete binaries first.
	const delParts = `
DELETE FROM parts
WHERE binary_id IN (
    SELECT id FROM binaries
    WHERE complete = FALSE AND released = FALSE AND updated_at < $1
)`
	if _, err := tx.Exec(ctx, delParts, cutoff); err != nil {
		return 0, fmt.Errorf("delete stale parts: %w", err)
	}

	const delBins = `
DELETE FROM binaries
WHERE complete = FALSE AND released = FALSE AND updated_at < $1`
	ct, err := tx.Exec(ctx, delBins, cutoff)
	if err != nil {
		return 0, fmt.Errorf("delete stale binaries: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit age-out: %w", err)
	}
	return ct.RowsAffected(), nil
}

// MarkBinariesReleased flags binaries as promoted to releases.
func (s *Store) MarkBinariesReleased(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := s.pool.Exec(ctx,
		`UPDATE binaries SET released = TRUE, updated_at = now() WHERE id = ANY($1)`, ids)
	if err != nil {
		return fmt.Errorf("mark released: %w", err)
	}
	return nil
}
