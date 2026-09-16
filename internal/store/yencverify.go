package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// MaxYencAttempts bounds how many times a part's yEnc header fetch is retried
// before giving up on determining its true segment count.
const MaxYencAttempts = 5

// YencCandidate is one part awaiting yEnc-header verification (#178): its
// Subject carried no segment counter, so its true completeness is unknown
// until the article body itself is inspected.
type YencCandidate struct {
	ID        int64
	MessageID string
}

// ListPartsNeedingYencVerification returns up to limit unassigned parts whose
// Subject carried no segment counter (total_parts = 0) and have not exhausted
// their verification attempts.
//
// Newest first: verification needs the article to still be retrievable, and
// the oldest end of this backlog is largely ancient text posts and articles
// long expired at the provider. Working forward from the oldest spent every
// pass on candidates that can never resolve, while freshly scanned parts —
// the ones both recoverable and actually wanted — waited behind millions of
// them.
func (s *Store) ListPartsNeedingYencVerification(ctx context.Context, limit int) ([]YencCandidate, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := s.pool.Query(ctx, `
SELECT id, message_id
FROM parts
WHERE binary_id IS NULL AND collection_key = '' AND total_parts = 0
  AND yenc_attempts < $2
ORDER BY id DESC
LIMIT $1`, limit, MaxYencAttempts)
	if err != nil {
		return nil, fmt.Errorf("list parts needing yenc verification: %w", err)
	}
	defer rows.Close()
	var out []YencCandidate
	for rows.Next() {
		var c YencCandidate
		if err := rows.Scan(&c.ID, &c.MessageID); err != nil {
			return nil, fmt.Errorf("scan yenc candidate: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// YencResult is the outcome of inspecting one part's article body against its
// yEnc header.
type YencResult struct {
	ID int64
	// OK is false if the fetch or yEnc parse failed (transient — retried up to
	// MaxYencAttempts).
	OK bool
	// Total is the true segment count from the yEnc header (1 for a genuinely
	// standalone post).
	Total int
	// Part is this article's 1-based position within the collection.
	Part int
	// Name is the yEnc name= field: a stable id shared by every segment of the
	// same file, present in the body even when the Subject is obfuscated.
	Name string
}

// ApplyYencVerification records the outcome of a verification batch. A
// standalone result (Total<=1) sets total_parts=1 so the part flows through
// the ordinary single-file assembly path; a multi-segment result populates
// collection_key/file_number/collection_files (keyed on the yEnc name rather
// than the Subject) so it flows through collection assembly and waits for its
// siblings. A failed fetch increments yenc_attempts for a bounded retry.
func (s *Store) ApplyYencVerification(ctx context.Context, results []YencResult) error {
	if len(results) == 0 {
		return nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	for _, r := range results {
		switch {
		case !r.OK:
			if _, err := tx.Exec(ctx,
				`UPDATE parts SET yenc_attempts = yenc_attempts + 1 WHERE id = $1`, r.ID); err != nil {
				return fmt.Errorf("record yenc failure: %w", err)
			}
		case r.Total <= 1:
			if _, err := tx.Exec(ctx,
				`UPDATE parts SET total_parts = 1 WHERE id = $1`, r.ID); err != nil {
				return fmt.Errorf("record yenc standalone: %w", err)
			}
		default:
			key := fmt.Sprintf("yenc:%s/%d", r.Name, r.Total)
			if _, err := tx.Exec(ctx,
				`UPDATE parts SET collection_key = $2, file_number = $3, collection_files = $4 WHERE id = $1`,
				r.ID, key, r.Part, r.Total); err != nil {
				return fmt.Errorf("record yenc fragment: %w", err)
			}
		}
	}
	return tx.Commit(ctx)
}

// YencRepairCandidate is a previously-released "done" release whose Subject
// carried no segment counter, promoted before yEnc verification existed
// (#178), and not yet checked against its article's real yEnc header.
type YencRepairCandidate struct {
	ReleaseID int64
	BinaryID  int64
	GroupID   int64
	Poster    string
	// PartID/MessageID identify the one known article. PartID is 0 if the raw
	// part row has since been pruned by retention; MessageID is always
	// available (durably captured on the release's segments).
	PartID    int64
	MessageID string
}

// ListReleasesNeedingYencRepair returns up to limit already-released stub
// releases (total_parts = 0, pp_status = 'done') not yet checked against their
// real yEnc header. The candidate's message-id comes from the release's
// durable segments (releases.segments), so this works even if the raw part
// was already pruned by retention; PartID is included when the raw row still
// exists, since repairing a genuine fragment needs to reset it.
//
// Candidate ids are selected in their own CTE so the partial index
// idx_releases_yenc_repair drives the scan. Selecting them alongside the parts
// join let the planner reach for releases_pkey instead — walking the table in
// id order on the assumption that "ORDER BY id LIMIT n" would stop early. Once
// few or no stubs remain that assumption inverts: the scan runs to completion
// every pass, discarding millions of rows to return nothing, on a task that
// repeats every minute.
func (s *Store) ListReleasesNeedingYencRepair(ctx context.Context, limit int) ([]YencRepairCandidate, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, `
WITH due AS (
    SELECT id FROM releases
    WHERE total_parts = 0 AND pp_status = 'done' AND yenc_verified = FALSE
    ORDER BY id
    LIMIT $1
)
SELECT r.id, r.binary_id, r.group_id, r.poster,
       coalesce(p.id, 0),
       r.segments->0->>'message_id'
FROM due
JOIN releases r ON r.id = due.id
LEFT JOIN parts p ON p.binary_id = r.binary_id
WHERE r.segments <> '[]'::jsonb`, limit)
	if err != nil {
		return nil, fmt.Errorf("list releases needing yenc repair: %w", err)
	}
	defer rows.Close()
	var out []YencRepairCandidate
	for rows.Next() {
		var c YencRepairCandidate
		if err := rows.Scan(&c.ReleaseID, &c.BinaryID, &c.GroupID, &c.Poster, &c.PartID, &c.MessageID); err != nil {
			return nil, fmt.Errorf("scan yenc repair candidate: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// YencRepairResult is the outcome of checking one already-released stub
// release against its article's real yEnc header.
type YencRepairResult struct {
	ReleaseID int64
	BinaryID  int64
	PartID    int64 // 0 if the raw part row no longer exists
	// OK is false if the fetch or yEnc parse failed (transient).
	OK bool
	// Total/Part/Name mirror YencResult; only meaningful when OK.
	Total int
	Part  int
	Name  string
}

// ApplyYencRepair reconciles the outcome of a repair batch. A confirmed
// standalone release (Total<=1) is just marked verified and left alone. A
// confirmed fragment (Total>1) is wrong: its release and binary are removed
// so the content can be properly reassembled, and — if the raw part row still
// exists — it is reset to unassigned with the discovered collection key so
// the ordinary assembler picks it up once its siblings are present. If the
// raw part was already pruned by retention, the release/binary are still
// removed (the stub NZB was wrong either way), but the segment itself cannot
// be recovered without a fresh scan; this is logged by the caller, not here.
// A failed fetch just leaves yenc_verified unset for a later retry.
func (s *Store) ApplyYencRepair(ctx context.Context, results []YencRepairResult) error {
	if len(results) == 0 {
		return nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	for _, r := range results {
		if !r.OK {
			continue // leave yenc_verified = false; picked up again next pass
		}
		if r.Total <= 1 {
			if _, err := tx.Exec(ctx,
				`UPDATE releases SET yenc_verified = TRUE WHERE id = $1`, r.ReleaseID); err != nil {
				return fmt.Errorf("mark release verified: %w", err)
			}
			continue
		}
		if err := discardStubRelease(ctx, tx, r); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// discardStubRelease removes a release/binary pair that was only ever one
// segment of a larger post, and resets the raw part (when it still exists) so
// the assembler can regroup it under its true yEnc collection key.
func discardStubRelease(ctx context.Context, tx pgx.Tx, r YencRepairResult) error {
	if _, err := tx.Exec(ctx, `DELETE FROM release_files WHERE release_id = $1`, r.ReleaseID); err != nil {
		return fmt.Errorf("delete release files: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM releases WHERE id = $1`, r.ReleaseID); err != nil {
		return fmt.Errorf("delete stub release: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM binaries WHERE id = $1`, r.BinaryID); err != nil {
		return fmt.Errorf("delete stub binary: %w", err)
	}
	if r.PartID == 0 {
		return nil
	}
	key := fmt.Sprintf("yenc:%s/%d", r.Name, r.Total)
	if _, err := tx.Exec(ctx,
		`UPDATE parts SET binary_id = NULL, collection_key = $2, file_number = $3, collection_files = $4
         WHERE id = $1`,
		r.PartID, key, r.Part, r.Total); err != nil {
		return fmt.Errorf("reset fragment part: %w", err)
	}
	return nil
}
