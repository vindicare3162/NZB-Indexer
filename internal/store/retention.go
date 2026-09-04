package store

import (
	"context"
	"fmt"
	"time"
)

// Raw-part retention (#118).
//
// Over an installation's lifetime the raw `parts` table grows without bound:
// once parts are folded into a binary and the binary is released, the article
// rows are never removed. But a released binary's ordered segments are captured
// durably on the release (releases.segments, #105), so for a release that is
// fully post-processed and reconstructable, its raw parts are redundant and can
// be pruned without breaking NZB generation.
//
// A part is a deletion CANDIDATE only when ALL of these hold, guaranteeing we
// never remove data still needed by active/incomplete work or a
// non-reconstructable release:
//
//   - it is assigned to a binary (binary_id IS NOT NULL);
//   - that binary is released (binaries.released = TRUE);
//   - the release built from it is fully post-processed (pp_status = 'done'),
//     so nothing pending/failed-retryable still needs the raw articles;
//   - the release has durable segments (releases.segments <> '[]'), so its NZB
//     is reconstructable without the raw parts;
//   - the release is older than the retention cutoff (by posted_at, falling
//     back to created_at when the post carried no date).
//
// Everything else is retained: unassigned parts (assembler backlog), parts of
// incomplete or unreleased binaries, parts of releases still pending/failed
// post-processing, and parts of releases without durable segments.

// retentionCandidateWhere is the shared predicate identifying prunable parts.
// It is parameterised by the cutoff time ($1).
const retentionCandidateWhere = `
p.binary_id IS NOT NULL
AND EXISTS (
    SELECT 1
    FROM binaries b
    JOIN releases r ON r.binary_id = b.id
    WHERE b.id = p.binary_id
      AND b.released = TRUE
      AND r.pp_status = 'done'
      AND r.segments <> '[]'::jsonb
      AND coalesce(r.posted_at, r.created_at) < $1
)`

// RetentionReport summarises the parts a retention pass would delete (dry-run),
// plus why the bulk of parts are retained.
type RetentionReport struct {
	// Cutoff is the age boundary used (parts for releases older than this are
	// candidates).
	Cutoff time.Time `json:"cutoff"`
	// CandidateParts is how many raw parts would be deleted.
	CandidateParts int64 `json:"candidate_parts"`
	// CandidateBytes is the summed `bytes` of those parts (estimated reclaimable
	// article payload; on-disk savings also include index/tuple overhead).
	CandidateBytes int64 `json:"candidate_bytes"`
	// OldestCandidate / NewestCandidate bound the candidate set by post date.
	OldestCandidate *time.Time `json:"oldest_candidate,omitempty"`
	NewestCandidate *time.Time `json:"newest_candidate,omitempty"`
	// CandidateReleases / CandidateGroups are how many distinct releases and
	// groups the candidate parts span.
	CandidateReleases int64 `json:"candidate_releases"`
	CandidateGroups   int64 `json:"candidate_groups"`
	// Retained explains why non-candidate parts are kept, keyed by reason.
	Retained RetentionRetained `json:"retained"`
}

// RetentionRetained breaks down retained parts by reason.
type RetentionRetained struct {
	// Unassigned parts not yet folded into a binary (assembler backlog).
	Unassigned int64 `json:"unassigned"`
	// Assigned parts whose binary/release is not yet safely prunable (incomplete
	// or unreleased binary, release not done, or no durable segments). This is
	// the "still needed / not reconstructable" bucket.
	NotReconstructable int64 `json:"not_reconstructable"`
}

// RetentionCandidates produces a dry-run report for the given retention age
// without deleting anything.
func (s *Store) RetentionCandidates(ctx context.Context, olderThan time.Duration) (RetentionReport, error) {
	cutoff := time.Now().Add(-olderThan)
	rep := RetentionReport{Cutoff: cutoff}

	// Candidate aggregate in one scan.
	const aggQ = `
SELECT
    count(*)                              AS parts,
    coalesce(sum(p.bytes), 0)             AS bytes,
    count(DISTINCT p.binary_id)           AS binaries,
    count(DISTINCT p.group_id)            AS groups,
    min(p.posted_at)                      AS oldest,
    max(p.posted_at)                      AS newest
FROM parts p
WHERE ` + retentionCandidateWhere
	var oldest, newest *time.Time
	if err := s.pool.QueryRow(ctx, aggQ, cutoff).Scan(
		&rep.CandidateParts, &rep.CandidateBytes, &rep.CandidateReleases,
		&rep.CandidateGroups, &oldest, &newest,
	); err != nil {
		return rep, fmt.Errorf("retention candidate aggregate: %w", err)
	}
	rep.OldestCandidate = oldest
	rep.NewestCandidate = newest

	// Retained: unassigned backlog.
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM parts WHERE binary_id IS NULL`,
	).Scan(&rep.Retained.Unassigned); err != nil {
		return rep, fmt.Errorf("retention unassigned count: %w", err)
	}

	// Retained: assigned but not (yet) safely prunable.
	const notReconQ = `
SELECT count(*)
FROM parts p
WHERE p.binary_id IS NOT NULL
  AND NOT (` + retentionCandidateWhere + `)`
	if err := s.pool.QueryRow(ctx, notReconQ, cutoff).Scan(&rep.Retained.NotReconstructable); err != nil {
		return rep, fmt.Errorf("retention not-reconstructable count: %w", err)
	}

	return rep, nil
}

// PruneRetainedParts deletes up to batchSize candidate parts in a single
// bounded transaction and returns how many were deleted. It is resumable and
// cancellable: call it repeatedly until it returns 0. Each call is its own
// transaction, so it never holds one unbounded transaction and can be
// interrupted (via ctx) between batches without leaving work half-done.
func (s *Store) PruneRetainedParts(ctx context.Context, olderThan time.Duration, batchSize int) (int64, error) {
	if batchSize <= 0 {
		batchSize = 5000
	}
	cutoff := time.Now().Add(-olderThan)

	// Delete a bounded set of candidate part ids. Selecting ids first keeps the
	// delete's row lock scope small and the batch strictly bounded.
	const q = `
DELETE FROM parts
WHERE id IN (
    SELECT p.id
    FROM parts p
    WHERE ` + retentionCandidateWhere + `
    ORDER BY p.id
    LIMIT $2
)`
	ct, err := s.pool.Exec(ctx, q, cutoff, batchSize)
	if err != nil {
		return 0, fmt.Errorf("prune retained parts: %w", err)
	}
	return ct.RowsAffected(), nil
}

// PruneRetainedPartsAll runs PruneRetainedParts in batches until no candidates
// remain or maxBatches is reached (0 = unlimited), returning the total deleted.
// It stops early if ctx is cancelled, returning what it managed to delete.
func (s *Store) PruneRetainedPartsAll(ctx context.Context, olderThan time.Duration, batchSize, maxBatches int) (int64, error) {
	var total int64
	for i := 0; maxBatches == 0 || i < maxBatches; i++ {
		if err := ctx.Err(); err != nil {
			return total, nil // cancelled: report progress so far
		}
		n, err := s.PruneRetainedParts(ctx, olderThan, batchSize)
		if err != nil {
			return total, err
		}
		total += n
		if n == 0 {
			break
		}
	}
	return total, nil
}
