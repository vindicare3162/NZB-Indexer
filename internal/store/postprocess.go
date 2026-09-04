package store

import (
	"context"
	"encoding/json"
	"fmt"
)

// PendingRelease is a release awaiting post-processing, with the message-ids
// of its candidate NFO/PAR2 segments.
type PendingRelease struct {
	Release  Release
	Segments []PartSegment
}

// MaxPPAttempts bounds how many times a release is retried through
// post-processing before it is left failed. It covers transient fetch failures
// without retrying genuinely-unrecoverable releases forever.
const MaxPPAttempts = 3

// ListPendingReleases returns releases due for post-processing: those still
// 'pending', plus 'failed' releases that have not yet exhausted their retry
// budget (a previous pass hit a transient fetch failure). It atomically
// increments pp_attempts for each release it returns, so concurrent workers and
// overlapping passes do not process the same release repeatedly, and a release
// that keeps failing eventually stops being retried. Each release comes with
// its part segments attached so the post-processor can locate PAR2/NFO articles.
func (s *Store) ListPendingReleases(ctx context.Context, limit int) ([]PendingRelease, error) {
	if limit <= 0 {
		limit = 100
	}
	// Claim a batch of due releases and bump their attempt counter in one
	// statement. 'pending' releases are always due; 'failed' ones are retried
	// until they reach MaxPPAttempts.
	rows, err := s.pool.Query(ctx, `
WITH due AS (
    SELECT id
    FROM releases
    WHERE pp_status = 'pending'
       OR (pp_status = 'failed' AND pp_attempts < $2)
    ORDER BY (pp_status = 'pending') DESC, created_at DESC
    LIMIT $1
)
UPDATE releases r
SET pp_attempts = r.pp_attempts + 1, updated_at = now()
FROM due
WHERE r.id = due.id
RETURNING r.id, r.guid, r.name, r.original_subject, r.search_name, r.category_id,
          r.group_id, r.binary_id, r.poster, r.total_parts, r.size_bytes,
          r.posted_at, r.release_hash, r.pp_status, r.nfo, r.grabs,
          r.created_at, r.updated_at`, limit, MaxPPAttempts)
	if err != nil {
		return nil, fmt.Errorf("list pending releases: %w", err)
	}

	var out []PendingRelease
	var ids []int64
	relByID := map[int64]int{} // release id -> index in out
	for rows.Next() {
		var r Release
		if err := rows.Scan(&r.ID, &r.GUID, &r.Name, &r.OriginalSubject, &r.SearchName,
			&r.CategoryID, &r.GroupID, &r.BinaryID, &r.Poster, &r.TotalParts, &r.SizeBytes,
			&r.PostedAt, &r.ReleaseHash, &r.PPStatus, &r.NFO, &r.Grabs,
			&r.CreatedAt, &r.UpdatedAt); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan pending release: %w", err)
		}
		relByID[r.ID] = len(out)
		out = append(out, PendingRelease{Release: r})
		ids = append(ids, r.ID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, nil
	}

	// Attach segments for all releases in one query.
	segRows, err := s.pool.Query(ctx, `
SELECT r.id, p.message_id, p.bytes, p.part_number, p.subject
FROM parts p
JOIN releases r ON r.binary_id = p.binary_id
WHERE r.id = ANY($1) AND p.message_id <> ''
ORDER BY r.id, p.part_number, p.article_number`, ids)
	if err != nil {
		return nil, fmt.Errorf("load pending segments: %w", err)
	}
	defer segRows.Close()
	for segRows.Next() {
		var relID int64
		var seg PartSegment
		if err := segRows.Scan(&relID, &seg.MessageID, &seg.Bytes, &seg.PartNumber, &seg.Subject); err != nil {
			return nil, fmt.Errorf("scan segment: %w", err)
		}
		if i, ok := relByID[relID]; ok {
			out[i].Segments = append(out[i].Segments, seg)
		}
	}
	return out, segRows.Err()
}

// RequeueFailedReleases moves every 'failed' release back into the
// post-processing queue by resetting it to 'pending' and clearing its attempt
// counter, so the next pass reprocesses it. It returns the number of releases
// reset. Intended for an operator "retry failed" action after a temporary
// provider problem is resolved.
func (s *Store) RequeueFailedReleases(ctx context.Context) (int64, error) {
	ct, err := s.pool.Exec(ctx,
		`UPDATE releases SET pp_status = 'pending', pp_attempts = 0, updated_at = now() WHERE pp_status = 'failed'`)
	if err != nil {
		return 0, fmt.Errorf("requeue failed releases: %w", err)
	}
	return ct.RowsAffected(), nil
}

// SetReleasePPStatus updates a release's post-processing status.
func (s *Store) SetReleasePPStatus(ctx context.Context, id int64, status string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE releases SET pp_status = $2, updated_at = now() WHERE id = $1`, id, status)
	if err != nil {
		return fmt.Errorf("set pp status: %w", err)
	}
	return nil
}

// ReleasePPResult carries the outcome of post-processing a release.
type ReleasePPResult struct {
	// Name, when non-empty, replaces the release name (recovered from PAR2).
	Name string
	// SearchName is the normalized search form of Name.
	SearchName string
	// CategoryID, when non-nil, replaces the release category. Post-processing
	// sets this when a recovered name yields a better categorization than the
	// (possibly obfuscated) name the release was built with.
	CategoryID *int
	// NFO, when non-nil, is stored as the release NFO text.
	NFO *string
	// Files, when non-empty, are stored as release_files.
	Files []ReleaseFileInput
}

// ReleaseFileInput describes a file recovered during post-processing.
type ReleaseFileInput struct {
	FileName  string
	SizeBytes int64
	Segments  []Segment
}

// ApplyPostProcessing writes the post-processing outcome for a release in one
// transaction: optional rename, optional NFO, optional release_files, and sets
// pp_status to 'done'.
func (s *Store) ApplyPostProcessing(ctx context.Context, id int64, res ReleasePPResult) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if res.Name != "" {
		// Post-processing only sends a recovered name that is genuinely readable
		// (it rejects PAR2 names that are themselves obfuscated), so clearing
		// the obfuscated flag here is safe.
		if _, err := tx.Exec(ctx,
			`UPDATE releases SET name = $2, search_name = $3, obfuscated = false, updated_at = now() WHERE id = $1`,
			id, res.Name, res.SearchName); err != nil {
			return fmt.Errorf("rename release: %w", err)
		}
	}
	if res.CategoryID != nil {
		if _, err := tx.Exec(ctx,
			`UPDATE releases SET category_id = $2, updated_at = now() WHERE id = $1`,
			id, *res.CategoryID); err != nil {
			return fmt.Errorf("recategorize release: %w", err)
		}
	}
	if res.NFO != nil {
		if _, err := tx.Exec(ctx,
			`UPDATE releases SET nfo = $2, updated_at = now() WHERE id = $1`, id, *res.NFO); err != nil {
			return fmt.Errorf("set nfo: %w", err)
		}
	}
	for _, f := range res.Files {
		segJSON, err := json.Marshal(f.Segments)
		if err != nil {
			return fmt.Errorf("marshal segments: %w", err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO release_files (release_id, file_name, size_bytes, segments)
             VALUES ($1, $2, $3, $4)`,
			id, f.FileName, f.SizeBytes, segJSON); err != nil {
			return fmt.Errorf("insert release file: %w", err)
		}
	}
	if _, err := tx.Exec(ctx,
		`UPDATE releases SET pp_status = 'done', updated_at = now() WHERE id = $1`, id); err != nil {
		return fmt.Errorf("finalize pp status: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit post-processing: %w", err)
	}
	return nil
}

// GetReleaseFiles returns the release_files rows for a release.
func (s *Store) GetReleaseFiles(ctx context.Context, releaseID int64) ([]ReleaseFile, error) {
	rows, err := s.pool.Query(ctx, `
SELECT id, release_id, file_name, size_bytes, segments, created_at
FROM release_files WHERE release_id = $1 ORDER BY file_name`, releaseID)
	if err != nil {
		return nil, fmt.Errorf("get release files: %w", err)
	}
	defer rows.Close()

	var out []ReleaseFile
	for rows.Next() {
		var f ReleaseFile
		var segJSON []byte
		if err := rows.Scan(&f.ID, &f.ReleaseID, &f.FileName, &f.SizeBytes, &segJSON, &f.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan release file: %w", err)
		}
		if len(segJSON) > 0 {
			if err := json.Unmarshal(segJSON, &f.Segments); err != nil {
				return nil, fmt.Errorf("unmarshal segments: %w", err)
			}
		}
		out = append(out, f)
	}
	return out, rows.Err()
}
