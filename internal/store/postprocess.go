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

// ListPendingReleases returns releases whose post-processing status is
// 'pending', newest first, up to limit, each with its part segments attached
// so the post-processor can locate PAR2/NFO articles.
func (s *Store) ListPendingReleases(ctx context.Context, limit int) ([]PendingRelease, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `
SELECT id, guid, name, original_subject, search_name, category_id, group_id, binary_id,
       poster, total_parts, size_bytes, posted_at, release_hash, pp_status,
       nfo, grabs, created_at, updated_at
FROM releases
WHERE pp_status = 'pending'
ORDER BY created_at DESC
LIMIT $1`, limit)
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
		if _, err := tx.Exec(ctx,
			`UPDATE releases SET name = $2, search_name = $3, updated_at = now() WHERE id = $1`,
			id, res.Name, res.SearchName); err != nil {
			return fmt.Errorf("rename release: %w", err)
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
