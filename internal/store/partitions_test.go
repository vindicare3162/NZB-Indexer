package store

import (
	"context"
	"testing"
	"time"
)

// makePartsPartitioned converts the fresh schema's `parts` into a RANGE
// partitioned table by created_at, mirroring the documented rollout. The
// natural key gains created_at (required for a unique constraint on a
// partitioned table). Called by partition tests only.
func makePartsPartitioned(t *testing.T, st *Store) {
	t.Helper()
	ctx := context.Background()
	stmts := []string{
		`DROP TABLE IF EXISTS parts CASCADE`,
		`CREATE TABLE parts (
            id              BIGSERIAL,
            group_id        BIGINT NOT NULL REFERENCES groups (id) ON DELETE CASCADE,
            article_number  BIGINT NOT NULL,
            message_id      TEXT NOT NULL,
            subject         TEXT NOT NULL,
            poster          TEXT NOT NULL DEFAULT '',
            posted_at       TIMESTAMPTZ,
            bytes           BIGINT NOT NULL DEFAULT 0,
            part_number     INTEGER NOT NULL DEFAULT 0,
            total_parts     INTEGER NOT NULL DEFAULT 0,
            norm_subject    TEXT NOT NULL DEFAULT '',
            binary_id       BIGINT,
            collection_key   TEXT NOT NULL DEFAULT '',
            file_number      INTEGER NOT NULL DEFAULT 0,
            collection_files INTEGER NOT NULL DEFAULT 0,
            created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
            PRIMARY KEY (id, created_at),
            UNIQUE (group_id, article_number, created_at)
        ) PARTITION BY RANGE (created_at)`,
		`CREATE INDEX idx_parts_binary ON parts (binary_id)`,
	}
	for _, q := range stmts {
		if _, err := st.pool.Exec(ctx, q); err != nil {
			t.Fatalf("partition setup %q: %v", q, err)
		}
	}
}

func TestPartsIsPartitionedDetection(t *testing.T) {
	st := freshStore(t)
	ctx := context.Background()

	// Fresh schema: parts is a plain table.
	if p, err := st.PartsIsPartitioned(ctx); err != nil || p {
		t.Fatalf("fresh parts should not be partitioned: %v %v", p, err)
	}
	// Ensure/Check are safe no-ops on an unpartitioned table.
	if n, err := st.EnsurePartsPartitions(ctx, time.Now(), 2); err != nil || n != 0 {
		t.Errorf("ensure on unpartitioned = (%d,%v), want (0,nil)", n, err)
	}
	if err := st.CheckPartsPartitionCoverage(ctx, time.Now()); err != nil {
		t.Errorf("coverage check on unpartitioned should be nil, got %v", err)
	}

	makePartsPartitioned(t, st)
	if p, err := st.PartsIsPartitioned(ctx); err != nil || !p {
		t.Fatalf("converted parts should be partitioned: %v %v", p, err)
	}
}

func TestEnsureAndCoverageAndRouting(t *testing.T) {
	st := freshStore(t)
	ctx := context.Background()
	makePartsPartitioned(t, st)
	g, _ := st.UpsertGroup(ctx, "alt.binaries.part", true)

	now := time.Date(2026, 2, 15, 12, 0, 0, 0, time.UTC)

	// No partitions yet: coverage check must report the gap (actionable).
	if err := st.CheckPartsPartitionCoverage(ctx, now); err == nil {
		t.Error("expected coverage error when no partition covers now")
	}

	// Create current + 2 future months.
	created, err := st.EnsurePartsPartitions(ctx, now, 2)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if created != 3 {
		t.Errorf("created = %d, want 3 (Feb, Mar, Apr)", created)
	}
	// Idempotent: a second ensure creates nothing.
	if created2, err := st.EnsurePartsPartitions(ctx, now, 2); err != nil || created2 != 0 {
		t.Errorf("second ensure = (%d,%v), want (0,nil)", created2, err)
	}
	if err := st.CheckPartsPartitionCoverage(ctx, now); err != nil {
		t.Errorf("coverage should be present after ensure, got %v", err)
	}

	// Routing: a row created "now" lands in the Feb partition.
	if _, err := st.pool.Exec(ctx,
		`INSERT INTO parts (group_id, article_number, message_id, subject, norm_subject, created_at)
         VALUES ($1, 1, 'm1@x', 's', 'n', $2)`, g.ID, now); err != nil {
		t.Fatalf("insert routed row: %v", err)
	}
	var febCount int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM parts_2026_02`).Scan(&febCount); err != nil {
		t.Fatal(err)
	}
	if febCount != 1 {
		t.Errorf("Feb partition rows = %d, want 1 (row routed by created_at)", febCount)
	}
}

func TestDropExpiredPartition(t *testing.T) {
	st := freshStore(t)
	ctx := context.Background()
	makePartsPartitioned(t, st)
	g, _ := st.UpsertGroup(ctx, "alt.binaries.expire", true)

	jan := time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC)
	feb := time.Date(2026, 2, 10, 0, 0, 0, 0, time.UTC)
	// Create Jan and Feb partitions.
	if _, err := st.EnsurePartsPartitions(ctx, jan, 1); err != nil { // Jan + Feb
		t.Fatal(err)
	}

	// One row in each month.
	for i, when := range []time.Time{jan, feb} {
		if _, err := st.pool.Exec(ctx,
			`INSERT INTO parts (group_id, article_number, message_id, subject, norm_subject, created_at)
             VALUES ($1, $2, 'm@x', 's', 'n', $3)`, g.ID, int64(i+1), when); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}

	var before int
	st.pool.QueryRow(ctx, `SELECT count(*) FROM parts`).Scan(&before)
	if before != 2 {
		t.Fatalf("expected 2 rows across partitions, got %d", before)
	}

	// Expire everything before Feb 1: only the Jan partition (upper bound
	// Feb 1) is fully older, so only it is dropped. Feb data is retained.
	cutoff := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	dropped, err := st.DropExpiredPartsPartitions(ctx, cutoff)
	if err != nil {
		t.Fatalf("drop expired: %v", err)
	}
	if len(dropped) != 1 || dropped[0] != "parts_2026_01" {
		t.Fatalf("dropped = %v, want [parts_2026_01]", dropped)
	}

	var after int
	st.pool.QueryRow(ctx, `SELECT count(*) FROM parts`).Scan(&after)
	if after != 1 {
		t.Errorf("after expiry rows = %d, want 1 (only Feb remains)", after)
	}
	// The straddling/current partition (Feb) must NOT have been dropped.
	var febExists bool
	st.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_class WHERE relname='parts_2026_02')`).Scan(&febExists)
	if !febExists {
		t.Error("Feb partition was dropped but should be retained")
	}
}

func TestListPartsPartitions(t *testing.T) {
	st := freshStore(t)
	ctx := context.Background()
	makePartsPartitioned(t, st)
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	if _, err := st.EnsurePartsPartitions(ctx, now, 1); err != nil {
		t.Fatal(err)
	}
	parts, err := st.ListPartsPartitions(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(parts) != 2 {
		t.Fatalf("partitions = %d, want 2", len(parts))
	}
	// Bounds parsed correctly for the first (June) partition.
	if parts[0].Name != "parts_2026_06" {
		t.Errorf("first partition = %s, want parts_2026_06", parts[0].Name)
	}
	if !parts[0].From.Equal(time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("June From = %v, want 2026-06-01", parts[0].From)
	}
	if !parts[0].To.Equal(time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("June To = %v, want 2026-07-01", parts[0].To)
	}
}
