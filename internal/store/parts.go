package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// PartInput is the data needed to insert one part row.
type PartInput struct {
	GroupID       int64
	ArticleNumber int64
	MessageID     string
	Subject       string
	Poster        string
	PostedAt      *time.Time
	Bytes         int64
	PartNumber    int
	TotalParts    int
	NormSubject   string
	// Collection metadata for multi-file posts. CollectionKey is empty for
	// single-file posts (which group by NormSubject).
	CollectionKey   string
	FileNumber      int
	CollectionFiles int
}

// InsertParts bulk-inserts parts within a single transaction. Rows conflicting
// on (group_id, article_number) are skipped, making re-scans idempotent. It
// returns the number of newly inserted rows.
func (s *Store) InsertParts(ctx context.Context, parts []PartInput) (int64, error) {
	if len(parts) == 0 {
		return 0, nil
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rolled back unless committed

	const q = `
INSERT INTO parts
    (group_id, article_number, message_id, subject, poster, posted_at, bytes, part_number, total_parts, norm_subject, collection_key, file_number, collection_files)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
ON CONFLICT (group_id, article_number) DO NOTHING`

	batch := &pgx.Batch{}
	for _, p := range parts {
		batch.Queue(q, p.GroupID, p.ArticleNumber, p.MessageID, p.Subject,
			p.Poster, p.PostedAt, p.Bytes, p.PartNumber, p.TotalParts, p.NormSubject,
			p.CollectionKey, p.FileNumber, p.CollectionFiles)
	}

	br := tx.SendBatch(ctx, batch)
	var inserted int64
	for range parts {
		ct, err := br.Exec()
		if err != nil {
			_ = br.Close()
			return 0, fmt.Errorf("insert part: %w", err)
		}
		inserted += ct.RowsAffected()
	}
	if err := br.Close(); err != nil {
		return 0, fmt.Errorf("close batch: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit parts: %w", err)
	}
	return inserted, nil
}

// CountParts returns the number of parts stored for a group.
func (s *Store) CountParts(ctx context.Context, groupID int64) (int64, error) {
	var n int64
	err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM parts WHERE group_id = $1`, groupID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count parts: %w", err)
	}
	return n, nil
}
