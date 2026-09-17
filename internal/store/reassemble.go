package store

import (
	"context"
	"fmt"
)

// Re-parsing and re-assembly of pre-fix parts (#196).
//
// ParseSubject runs once, when a part row is inserted, and its output is
// stored. A parser fix therefore only affects parts ingested after the fix is
// deployed; everything older keeps whatever grouping it was given. Measured
// against a sample of production subjects, ~31.6% of parts gain a collection
// key when re-parsed, and essentially all of them are already assembled — so a
// re-parse alone changes nothing a user can see. The binaries and releases have
// to be rebuilt too.
//
// Rather than reimplement assembly, this repairs the inputs and lets the normal
// assembler do its job: fix the part's grouping fields, detach it from its
// binary, and delete the now-empty binary. The assembler picks the parts up on
// its next pass and groups them correctly.
//
// Two invariants make that safe:
//
//   - releases.binary_id is ON DELETE SET NULL, not CASCADE. Deleting a binary
//     would leave its release behind with a null binary and an NZB that can no
//     longer be generated, so releases are removed explicitly first.
//   - A release that has been post-processed ('done') may already have been
//     grabbed by a client. Those binaries are skipped entirely — parts included
//     — so nothing a user has already seen changes identity.

// PartForReparse is the subset of a part needed to decide whether re-parsing
// changes its grouping.
type PartForReparse struct {
	ID              int64
	Subject         string
	BinaryID        *int64
	CollectionKey   string
	FileNumber      int
	CollectionFiles int
	NormSubject     string
}

// PartReparse is a computed grouping update for one part.
type PartReparse struct {
	ID              int64
	CollectionKey   string
	FileNumber      int
	CollectionFiles int
	NormSubject     string
}

// ListPartsForReparse returns up to limit parts with id > afterID, in id order.
// Keyset paging on the primary key keeps each batch an index range scan whose
// cost does not grow as the cursor advances — the property an offset-based scan
// over 1.8B rows would lose.
func (s *Store) ListPartsForReparse(ctx context.Context, afterID int64, limit int) ([]PartForReparse, error) {
	if limit <= 0 {
		limit = 5000
	}
	rows, err := s.pool.Query(ctx, `
SELECT id, subject, binary_id, collection_key, file_number, collection_files, norm_subject
FROM parts
WHERE id > $1
ORDER BY id
LIMIT $2`, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("list parts for reparse: %w", err)
	}
	defer rows.Close()

	var out []PartForReparse
	for rows.Next() {
		var p PartForReparse
		if err := rows.Scan(&p.ID, &p.Subject, &p.BinaryID, &p.CollectionKey,
			&p.FileNumber, &p.CollectionFiles, &p.NormSubject); err != nil {
			return nil, fmt.Errorf("scan part for reparse: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ReassembleStats reports what one batch changed.
type ReassembleStats struct {
	PartsUpdated      int64
	PartsDetached     int64
	BinariesDeleted   int64
	ReleasesDeleted   int64
	BinariesProtected int64
}

// ApplyReparse writes a batch of grouping updates and tears down the binaries
// they belonged to, in one transaction so a crash cannot leave parts pointing
// at a deleted binary.
//
// dirty lists the binaries whose parts changed. Any binary backing a release
// that has already been post-processed is left completely untouched, and the
// count is reported so the caller can see how much was deliberately skipped.
func (s *Store) ApplyReparse(ctx context.Context, updates []PartReparse, dirty []int64) (ReassembleStats, error) {
	var st ReassembleStats
	if len(updates) == 0 && len(dirty) == 0 {
		return st, nil
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return st, fmt.Errorf("begin reparse tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// Protect anything a client may already have grabbed. Determined inside the
	// transaction so a release that reaches 'done' concurrently is still seen.
	var protected []int64
	if len(dirty) > 0 {
		rows, err := tx.Query(ctx,
			`SELECT DISTINCT binary_id FROM releases
             WHERE binary_id = ANY($1) AND pp_status = 'done'`, dirty)
		if err != nil {
			return st, fmt.Errorf("find protected binaries: %w", err)
		}
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return st, fmt.Errorf("scan protected binary: %w", err)
			}
			protected = append(protected, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return st, err
		}
	}
	st.BinariesProtected = int64(len(protected))

	protectedSet := make(map[int64]bool, len(protected))
	for _, id := range protected {
		protectedSet[id] = true
	}

	// Apply grouping updates, skipping parts belonging to a protected binary.
	//
	// One set-based statement rather than a statement per row. A batch commonly
	// rewrites 20-35k parts, and issuing that many sequential round-trips made
	// the round-trips — not the scan or the parsing — the throughput ceiling for
	// the whole repair.
	if len(updates) > 0 {
		ids := make([]int64, len(updates))
		keys := make([]string, len(updates))
		fileNums := make([]int32, len(updates))
		fileTotals := make([]int32, len(updates))
		norms := make([]string, len(updates))
		for i, u := range updates {
			ids[i] = u.ID
			keys[i] = u.CollectionKey
			fileNums[i] = int32(u.FileNumber)
			fileTotals[i] = int32(u.CollectionFiles)
			norms[i] = u.NormSubject
		}
		ct, err := tx.Exec(ctx, `
UPDATE parts p SET
    collection_key   = u.collection_key,
    file_number      = u.file_number,
    collection_files = u.collection_files,
    norm_subject     = u.norm_subject
FROM unnest($1::bigint[], $2::text[], $3::int[], $4::int[], $5::text[])
     AS u(id, collection_key, file_number, collection_files, norm_subject)
WHERE p.id = u.id
  AND (p.binary_id IS NULL OR p.binary_id <> ALL($6))`,
			ids, keys, fileNums, fileTotals, norms, protected)
		if err != nil {
			return st, fmt.Errorf("update part grouping: %w", err)
		}
		st.PartsUpdated = ct.RowsAffected()
	}

	var teardown []int64
	for _, id := range dirty {
		if !protectedSet[id] {
			teardown = append(teardown, id)
		}
	}
	if len(teardown) == 0 {
		if err := tx.Commit(ctx); err != nil {
			return st, fmt.Errorf("commit reparse: %w", err)
		}
		return st, nil
	}

	// Detach first: parts must not reference a binary that is about to go.
	ct, err := tx.Exec(ctx, `UPDATE parts SET binary_id = NULL WHERE binary_id = ANY($1)`, teardown)
	if err != nil {
		return st, fmt.Errorf("detach parts: %w", err)
	}
	st.PartsDetached = ct.RowsAffected()

	// Then releases: the FK is ON DELETE SET NULL, so deleting the binary first
	// would strand the release with a null binary and an NZB it can no longer
	// generate.
	ct, err = tx.Exec(ctx, `DELETE FROM releases WHERE binary_id = ANY($1)`, teardown)
	if err != nil {
		return st, fmt.Errorf("delete stale releases: %w", err)
	}
	st.ReleasesDeleted = ct.RowsAffected()

	ct, err = tx.Exec(ctx, `DELETE FROM binaries WHERE id = ANY($1)`, teardown)
	if err != nil {
		return st, fmt.Errorf("delete stale binaries: %w", err)
	}
	st.BinariesDeleted = ct.RowsAffected()

	if err := tx.Commit(ctx); err != nil {
		return st, fmt.Errorf("commit reparse: %w", err)
	}
	return st, nil
}
