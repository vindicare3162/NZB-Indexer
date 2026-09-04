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

// partColumns is the column order used by both the COPY staging load and the
// final INSERT ... SELECT.
var partColumns = []string{
	"group_id", "article_number", "message_id", "subject", "poster", "posted_at",
	"bytes", "part_number", "total_parts", "norm_subject", "collection_key",
	"file_number", "collection_files",
}

// InsertParts bulk-loads parts within a single transaction using PostgreSQL
// COPY into a per-transaction staging table, then a set-based
// INSERT ... SELECT ... ON CONFLICT DO NOTHING into `parts` (#115). This is
// materially faster than one INSERT per article for high-volume header
// ingestion while preserving idempotency: rows conflicting on
// (group_id, article_number) — whether already stored or duplicated within the
// batch — are skipped. Returns the number of newly inserted rows.
//
// The whole load is one transaction: on any error (including ctx cancellation)
// nothing is committed, so a failed/cancelled batch inserts no parts and the
// caller must not advance its watermark. Memory is bounded by the caller's
// batch size; the staging table is TEMP ON COMMIT DROP so it never persists.
func (s *Store) InsertParts(ctx context.Context, parts []PartInput) (int64, error) {
	if len(parts) == 0 {
		return 0, nil
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rolled back unless committed

	// Staging table mirroring parts' loadable columns, with no constraints or
	// indexes so COPY is as fast as possible. Dropped on commit/rollback.
	if _, err := tx.Exec(ctx, `
CREATE TEMP TABLE parts_stage (
    group_id         BIGINT,
    article_number   BIGINT,
    message_id       TEXT,
    subject          TEXT,
    poster           TEXT,
    posted_at        TIMESTAMPTZ,
    bytes            BIGINT,
    part_number      INTEGER,
    total_parts      INTEGER,
    norm_subject     TEXT,
    collection_key   TEXT,
    file_number      INTEGER,
    collection_files INTEGER
) ON COMMIT DROP`); err != nil {
		return 0, fmt.Errorf("create staging table: %w", err)
	}

	// Bulk-load the batch via COPY.
	rows := make([][]any, len(parts))
	for i, p := range parts {
		rows[i] = []any{
			p.GroupID, p.ArticleNumber, p.MessageID, p.Subject, p.Poster, p.PostedAt,
			p.Bytes, p.PartNumber, p.TotalParts, p.NormSubject, p.CollectionKey,
			p.FileNumber, p.CollectionFiles,
		}
	}
	if _, err := tx.CopyFrom(ctx, pgx.Identifier{"parts_stage"}, partColumns,
		pgx.CopyFromRows(rows)); err != nil {
		return 0, fmt.Errorf("copy parts to staging: %w", err)
	}

	// Fold the staged rows into parts. DISTINCT ON collapses in-batch duplicates
	// on the natural key so ON CONFLICT never has to affect the same row twice;
	// ON CONFLICT DO NOTHING skips rows already present (idempotent re-scans).
	const insQ = `
INSERT INTO parts
    (group_id, article_number, message_id, subject, poster, posted_at, bytes, part_number, total_parts, norm_subject, collection_key, file_number, collection_files)
SELECT DISTINCT ON (group_id, article_number)
    group_id, article_number, message_id, subject, poster, posted_at, bytes, part_number, total_parts, norm_subject, collection_key, file_number, collection_files
FROM parts_stage
ORDER BY group_id, article_number
ON CONFLICT (group_id, article_number) DO NOTHING`
	ct, err := tx.Exec(ctx, insQ)
	if err != nil {
		return 0, fmt.Errorf("insert staged parts: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit parts: %w", err)
	}
	return ct.RowsAffected(), nil
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
