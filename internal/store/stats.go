package store

import (
	"context"
	"fmt"
)

// PipelineStats is a snapshot of current pipeline depth, for operator health
// monitoring. Parts counts are planner estimates (the parts table can hold
// tens of millions of rows, so exact counts are too expensive to run on every
// poll); binary and release counts are exact since those tables are far
// smaller.
type PipelineStats struct {
	// PartsTotal is an estimate of all parts rows.
	PartsTotal int64 `json:"parts_total"`
	// PartsUnassigned is an estimate of parts not yet folded into a binary
	// (binary_id IS NULL) — the assembler backlog.
	PartsUnassigned int64 `json:"parts_unassigned"`

	BinariesTotal      int64 `json:"binaries_total"`
	BinariesComplete   int64 `json:"binaries_complete"`
	BinariesUnreleased int64 `json:"binaries_unreleased"` // complete AND not released

	ReleasesTotal   int64            `json:"releases_total"`
	ReleasesByPP    map[string]int64 `json:"releases_by_pp_status"`
}

// PipelineStatistics returns a cheap snapshot of current pipeline depth.
//
// The parts totals use PostgreSQL planner estimates (pg_class.reltuples and a
// partial-index estimate) rather than exact counts, so the query stays fast
// even with tens of millions of parts. Binary and release counts are exact.
func (s *Store) PipelineStatistics(ctx context.Context) (PipelineStats, error) {
	var out PipelineStats
	out.ReleasesByPP = map[string]int64{}

	// Estimated total parts from the planner statistics.
	if err := s.pool.QueryRow(ctx,
		`SELECT GREATEST(reltuples, 0)::bigint FROM pg_class WHERE relname = 'parts'`,
	).Scan(&out.PartsTotal); err != nil {
		return out, fmt.Errorf("estimate parts total: %w", err)
	}

	// Estimated unassigned parts via the planner's row estimate for the partial
	// grouping index idx_parts_grouping, which is defined WHERE binary_id IS
	// NULL and therefore covers exactly the assembler backlog. Using this one
	// index's estimate avoids scanning millions of rows.
	if err := s.pool.QueryRow(ctx,
		`SELECT coalesce(GREATEST(reltuples, 0), 0)::bigint FROM pg_class WHERE relname = 'idx_parts_grouping'`,
	).Scan(&out.PartsUnassigned); err != nil {
		return out, fmt.Errorf("estimate unassigned parts: %w", err)
	}

	// Exact binary counts (binaries is small relative to parts).
	if err := s.pool.QueryRow(ctx, `
SELECT
    count(*),
    count(*) FILTER (WHERE complete),
    count(*) FILTER (WHERE complete AND NOT released)
FROM binaries`,
	).Scan(&out.BinariesTotal, &out.BinariesComplete, &out.BinariesUnreleased); err != nil {
		return out, fmt.Errorf("count binaries: %w", err)
	}

	// Releases broken down by post-processing status, in one grouped scan; the
	// total is the sum of the groups (avoids a second full scan of releases).
	rows, err := s.pool.Query(ctx, `SELECT pp_status, count(*) FROM releases GROUP BY pp_status`)
	if err != nil {
		return out, fmt.Errorf("count releases by pp status: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var status string
		var n int64
		if err := rows.Scan(&status, &n); err != nil {
			return out, fmt.Errorf("scan pp status count: %w", err)
		}
		out.ReleasesByPP[status] = n
		out.ReleasesTotal += n
	}
	return out, rows.Err()
}
