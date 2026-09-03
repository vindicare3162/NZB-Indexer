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

	// A grouping is either a multi-file collection (keyed on collection_key) or
	// a plain single-file post (keyed on norm_subject). We process both.
	type key struct {
		groupID      int64
		groupingKey  string // collection_key or norm_subject
		poster       string
		isCollection bool
	}
	var keys []key

	// (1) Collection groupings: parts sharing a collection_key belong to one
	// multi-file post (rar volumes + PAR2 etc.) and fold into a single binary.
	// No ORDER BY: ordering the groupings is unnecessary for correctness, and
	// an "ORDER BY max(created_at)" would force a full aggregate+sort over the
	// entire unassigned backlog every pass. Without it the partial index
	// idx_parts_collection can satisfy the grouped scan cheaply.
	const selectCollections = `
SELECT group_id, collection_key, poster
FROM parts
WHERE binary_id IS NULL AND collection_key <> ''
GROUP BY group_id, collection_key, poster
LIMIT $1`
	crows, err := tx.Query(ctx, selectCollections, limit)
	if err != nil {
		return 0, fmt.Errorf("select collection groupings: %w", err)
	}
	for crows.Next() {
		k := key{isCollection: true}
		if err := crows.Scan(&k.groupID, &k.groupingKey, &k.poster); err != nil {
			crows.Close()
			return 0, fmt.Errorf("scan collection grouping: %w", err)
		}
		keys = append(keys, k)
	}
	crows.Close()
	if err := crows.Err(); err != nil {
		return 0, err
	}

	// (2) Single-file groupings: parts with no collection_key group by
	// norm_subject exactly as before, so single-file behaviour is unchanged.
	const selectSingles = `
SELECT group_id, norm_subject, poster
FROM parts
WHERE binary_id IS NULL AND collection_key = '' AND norm_subject <> ''
GROUP BY group_id, norm_subject, poster
LIMIT $1`
	srows, err := tx.Query(ctx, selectSingles, limit)
	if err != nil {
		return 0, fmt.Errorf("select single groupings: %w", err)
	}
	for srows.Next() {
		var k key
		if err := srows.Scan(&k.groupID, &k.groupingKey, &k.poster); err != nil {
			srows.Close()
			return 0, fmt.Errorf("scan single grouping: %w", err)
		}
		keys = append(keys, k)
	}
	srows.Close()
	if err := srows.Err(); err != nil {
		return 0, err
	}

	touched := 0
	for _, k := range keys {
		if k.isCollection {
			if err := assembleCollection(ctx, tx, k.groupID, k.groupingKey, k.poster); err != nil {
				return 0, err
			}
		} else {
			if err := assembleOne(ctx, tx, k.groupID, k.groupingKey, k.poster); err != nil {
				return 0, err
			}
		}
		touched++
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit assemble: %w", err)
	}
	return touched, nil
}

// assembleOne upserts the binary for one single-file grouping and links its
// parts. It runs inside the caller's transaction. Completeness for a
// single-file post is based on the yEnc segment count (total_parts).
func assembleOne(ctx context.Context, tx pgx.Tx, groupID int64, normSubject, poster string) error {
	// Aggregate the unassigned parts for this grouping.
	const agg = `
SELECT
    count(*)                              AS collected,
    coalesce(max(total_parts), 0)         AS declared_total,
    coalesce(sum(bytes), 0)               AS total_bytes,
    min(posted_at)                        AS earliest
FROM parts
WHERE group_id = $1 AND norm_subject = $2 AND poster = $3 AND binary_id IS NULL AND collection_key = ''`

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
-- Completeness: when the declared total is known (>0) the binary is complete
-- once all parts arrive; when the total is unknown (0) the post carried no
-- segment counter, i.e. a single-article file, which is complete with its one
-- part. collected is always >= 1 here (we skip empty groupings).
INSERT INTO binaries
    (group_id, norm_subject, poster, total_parts, collected_parts, total_bytes, posted_at, complete)
VALUES ($1, $2, $3, $4::int, $5::int, $6::bigint, $7,
        ($4::int = 0 OR $5::int >= $4::int))
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
WHERE group_id = $1 AND norm_subject = $2 AND poster = $3 AND binary_id IS NULL AND collection_key = ''`
	if _, err := tx.Exec(ctx, link, groupID, normSubject, poster, binaryID); err != nil {
		return fmt.Errorf("link parts: %w", err)
	}
	return nil
}

// assembleCollection folds all parts of a multi-file collection (identified by
// collection_key) into a single binary and links them. Completeness is judged
// by how many distinct files of the collection have been collected versus the
// declared file count, rather than by yEnc segment counts. The binary's
// norm_subject is set to the collection_key, which has a distinctive
// "base/count" form that does not collide with single-file norm_subjects, so
// the existing (group_id, norm_subject, poster) unique key still applies.
func assembleCollection(ctx context.Context, tx pgx.Tx, groupID int64, collectionKey, poster string) error {
	// Aggregate the unassigned parts for this collection. distinct_files counts
	// how many files (by file_number) are present; declared_files is the
	// collection's declared file count.
	const agg = `
SELECT
    count(*)                                        AS collected_segments,
    count(DISTINCT file_number)                     AS distinct_files,
    coalesce(max(collection_files), 0)              AS declared_files,
    coalesce(sum(bytes), 0)                         AS total_bytes,
    min(posted_at)                                  AS earliest
FROM parts
WHERE group_id = $1 AND collection_key = $2 AND poster = $3 AND binary_id IS NULL`

	var (
		collectedSegs int
		distinctFiles int
		declaredFiles int
		totalBytes    int64
		earliest      *time.Time
	)
	if err := tx.QueryRow(ctx, agg, groupID, collectionKey, poster).
		Scan(&collectedSegs, &distinctFiles, &declaredFiles, &totalBytes, &earliest); err != nil {
		return fmt.Errorf("aggregate collection parts: %w", err)
	}
	if collectedSegs == 0 {
		return nil
	}

	// Upsert the collection binary. total_parts here tracks the declared file
	// count (used for completeness); collected_parts accumulates distinct files
	// present. On conflict we add newly seen distinct files and re-evaluate
	// completeness against the declared file count.
	const upsert = `
INSERT INTO binaries
    (group_id, norm_subject, poster, total_parts, collected_parts, total_bytes, posted_at,
     collection_key, collection_files, complete)
VALUES ($1, $2, $3, $4::int, $5::int, $6::bigint, $7, $2, $4::int,
        ($5::int >= $4::int AND $4::int > 0))
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
RETURNING id`

	var binaryID int64
	if err := tx.QueryRow(ctx, upsert,
		groupID, collectionKey, poster, declaredFiles, distinctFiles, totalBytes, earliest,
	).Scan(&binaryID); err != nil {
		return fmt.Errorf("upsert collection binary: %w", err)
	}

	// Link every part of this collection to the binary.
	const link = `
UPDATE parts SET binary_id = $4
WHERE group_id = $1 AND collection_key = $2 AND poster = $3 AND binary_id IS NULL`
	if _, err := tx.Exec(ctx, link, groupID, collectionKey, poster, binaryID); err != nil {
		return fmt.Errorf("link collection parts: %w", err)
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
