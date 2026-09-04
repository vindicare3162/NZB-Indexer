package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// AssembleBinaries folds unassigned parts (binary_id IS NULL) into binaries,
// grouped by (group_id, norm_subject, poster). It upserts a binary row per
// grouping (accumulating collected_parts, total_bytes, and the max declared
// total_parts and earliest posted_at), then links the parts to that binary.
//
// It processes at most `limit` groupings of each kind (collection and
// single-file) per call to bound work, and returns the number of binaries
// touched. Empty norm_subject parts are ignored (they can't be grouped
// reliably).
//
// Set-based (#116): rather than issuing three statements per grouping in a Go
// loop (2 + 3N round trips per batch), the whole batch is folded with two
// single-statement CTE pipelines — one for collections, one for single-file
// posts. Each aggregates the selected groupings, bulk-upserts the binaries with
// ON CONFLICT DO UPDATE (same additive accumulation + completeness semantics as
// before), and bulk-links the parts by joining on the grouping key. This turns
// a batch of N groupings into a small constant number of round trips.
func (s *Store) AssembleBinaries(ctx context.Context, limit int) (int, error) {
	if limit <= 0 {
		limit = 500
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	collTouched, err := assembleCollectionsBatch(ctx, tx, limit)
	if err != nil {
		return 0, err
	}
	singleTouched, err := assembleSinglesBatch(ctx, tx, limit)
	if err != nil {
		return 0, err
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit assemble: %w", err)
	}
	return collTouched + singleTouched, nil
}

// assembleSinglesBatch folds up to `limit` single-file groupings (parts with no
// collection_key, grouped by norm_subject) in one set-based statement.
// Completeness for a single-file post is based on the yEnc segment count
// (total_parts): known total -> complete when all parts arrive; unknown total
// (0, a single-article file) -> complete with its one part. Returns the number
// of binaries touched.
//
// The CTE pipeline:
//   agg   — aggregate the selected unassigned single-file groupings.
//   ups   — bulk upsert into binaries, ON CONFLICT adding the newly collected
//           parts/bytes to any existing binary, keeping the larger declared
//           total and earliest posted_at, and recomputing completeness. The
//           RETURNING clause yields each binary's id alongside its grouping key
//           so the parts can be linked.
//   linked — set binary_id on exactly the parts that were aggregated, joining
//           on (group_id, norm_subject, poster). Only unassigned single-file
//           parts are touched, matching the original per-group link predicate.
func assembleSinglesBatch(ctx context.Context, tx pgx.Tx, limit int) (int, error) {
	const q = `
WITH agg AS (
    SELECT group_id, norm_subject, poster,
           count(*)                      AS collected,
           coalesce(max(total_parts), 0) AS declared_total,
           coalesce(sum(bytes), 0)       AS total_bytes,
           min(posted_at)                AS earliest
    FROM parts
    WHERE binary_id IS NULL AND collection_key = '' AND norm_subject <> ''
    GROUP BY group_id, norm_subject, poster
    LIMIT $1
),
ups AS (
    INSERT INTO binaries
        (group_id, norm_subject, poster, total_parts, collected_parts, total_bytes, posted_at, complete)
    SELECT group_id, norm_subject, poster, declared_total, collected, total_bytes, earliest,
           (declared_total = 0 OR collected >= declared_total)
    FROM agg
    ON CONFLICT (group_id, norm_subject, poster) DO UPDATE SET
        collected_parts = binaries.collected_parts + EXCLUDED.collected_parts,
        total_parts     = GREATEST(binaries.total_parts, EXCLUDED.total_parts),
        total_bytes     = binaries.total_bytes + EXCLUDED.total_bytes,
        posted_at       = LEAST(binaries.posted_at, EXCLUDED.posted_at),
        complete        = (
            GREATEST(binaries.total_parts, EXCLUDED.total_parts) = 0
            OR (binaries.collected_parts + EXCLUDED.collected_parts)
                >= GREATEST(binaries.total_parts, EXCLUDED.total_parts)
        ),
        updated_at      = now()
    RETURNING id, group_id, norm_subject, poster
),
linked AS (
    UPDATE parts p SET binary_id = u.id
    FROM ups u
    WHERE p.group_id = u.group_id AND p.norm_subject = u.norm_subject
      AND p.poster = u.poster AND p.binary_id IS NULL AND p.collection_key = ''
    RETURNING 1
)
SELECT (SELECT count(*) FROM ups)`
	var touched int
	if err := tx.QueryRow(ctx, q, limit).Scan(&touched); err != nil {
		return 0, fmt.Errorf("assemble single-file batch: %w", err)
	}
	return touched, nil
}

// assembleCollectionsBatch folds up to `limit` multi-file collections (parts
// sharing a collection_key) in one set-based statement. Each collection folds
// into a single binary whose completeness is judged by how many distinct files
// are present versus the declared file count. The binary's norm_subject is set
// to the collection_key (a distinctive "base/count" form that does not collide
// with single-file norm_subjects), so the (group_id, norm_subject, poster)
// unique key still applies. Returns the number of binaries touched.
func assembleCollectionsBatch(ctx context.Context, tx pgx.Tx, limit int) (int, error) {
	const q = `
WITH agg AS (
    SELECT group_id, collection_key, poster,
           count(DISTINCT file_number)        AS distinct_files,
           coalesce(max(collection_files), 0) AS declared_files,
           coalesce(sum(bytes), 0)            AS total_bytes,
           min(posted_at)                     AS earliest
    FROM parts
    WHERE binary_id IS NULL AND collection_key <> ''
    GROUP BY group_id, collection_key, poster
    LIMIT $1
),
ups AS (
    INSERT INTO binaries
        (group_id, norm_subject, poster, total_parts, collected_parts, total_bytes, posted_at,
         collection_key, collection_files, complete)
    SELECT group_id, collection_key, poster, declared_files, distinct_files, total_bytes, earliest,
           collection_key, declared_files,
           (distinct_files >= declared_files AND declared_files > 0)
    FROM agg
    ON CONFLICT (group_id, norm_subject, poster) DO UPDATE SET
        collected_parts  = binaries.collected_parts + EXCLUDED.collected_parts,
        total_parts      = GREATEST(binaries.total_parts, EXCLUDED.total_parts),
        collection_files = GREATEST(binaries.collection_files, EXCLUDED.collection_files),
        total_bytes      = binaries.total_bytes + EXCLUDED.total_bytes,
        posted_at        = LEAST(binaries.posted_at, EXCLUDED.posted_at),
        complete         = (
            (binaries.collected_parts + EXCLUDED.collected_parts)
                >= GREATEST(binaries.total_parts, EXCLUDED.total_parts)
            AND GREATEST(binaries.total_parts, EXCLUDED.total_parts) > 0
        ),
        updated_at       = now()
    RETURNING id, group_id, collection_key, poster
),
linked AS (
    UPDATE parts p SET binary_id = u.id
    FROM ups u
    WHERE p.group_id = u.group_id AND p.collection_key = u.collection_key
      AND p.poster = u.poster AND p.binary_id IS NULL
    RETURNING 1
)
SELECT (SELECT count(*) FROM ups)`
	var touched int
	if err := tx.QueryRow(ctx, q, limit).Scan(&touched); err != nil {
		return 0, fmt.Errorf("assemble collection batch: %w", err)
	}
	return touched, nil
}

// ListCompleteUnreleasedBinaries returns complete binaries not yet promoted to
// releases, up to limit.
func (s *Store) ListCompleteUnreleasedBinaries(ctx context.Context, limit int) ([]Binary, error) {
	if limit <= 0 {
		limit = 500
	}
	const q = `
SELECT id, group_id, norm_subject, poster, total_parts, collected_parts, total_bytes, posted_at, complete, released, created_at, updated_at, collection_key, collection_files
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
			&b.Complete, &b.Released, &b.CreatedAt, &b.UpdatedAt,
			&b.CollectionKey, &b.CollectionFiles); err != nil {
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
