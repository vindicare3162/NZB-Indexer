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

	ReleasesTotal int64            `json:"releases_total"`
	ReleasesByPP  map[string]int64 `json:"releases_by_pp_status"`
	// ReleasesFailedExhausted is the number of releases that are 'failed' and
	// have exhausted their post-processing retry budget (pp_attempts >=
	// MaxPPAttempts), i.e. permanently stuck rather than awaiting another retry.
	ReleasesFailedExhausted int64 `json:"releases_failed_exhausted"`

	// Groups is a per-group release breakdown (only groups that have releases),
	// ordered by release count descending.
	Groups []GroupReleaseStats `json:"groups"`
}

// GroupReleaseStats summarises one group's release counts.
type GroupReleaseStats struct {
	Name             string `json:"name"`
	ReleasesTotal    int64  `json:"releases_total"`
	ReleasesPending  int64  `json:"releases_pending"`
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
	if err := rows.Err(); err != nil {
		return out, err
	}

	// Failed releases that have exhausted their retry budget (permanently
	// stuck). Uses idx_releases_pp_retry, so it stays cheap.
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM releases WHERE pp_status = 'failed' AND pp_attempts >= $1`, MaxPPAttempts,
	).Scan(&out.ReleasesFailedExhausted); err != nil {
		return out, fmt.Errorf("count exhausted failed releases: %w", err)
	}

	// Per-group release breakdown from the (small) releases table joined to
	// group names. A single grouped scan; cheap relative to the parts table.
	grows, err := s.pool.Query(ctx, `
SELECT g.name,
       count(r.*)                                        AS total,
       count(r.*) FILTER (WHERE r.pp_status = 'pending') AS pending
FROM releases r
JOIN groups g ON g.id = r.group_id
GROUP BY g.name
ORDER BY total DESC`)
	if err != nil {
		return out, fmt.Errorf("per-group release stats: %w", err)
	}
	defer grows.Close()
	for grows.Next() {
		var gs GroupReleaseStats
		if err := grows.Scan(&gs.Name, &gs.ReleasesTotal, &gs.ReleasesPending); err != nil {
			return out, fmt.Errorf("scan group stats: %w", err)
		}
		out.Groups = append(out.Groups, gs)
	}
	return out, grows.Err()
}

// DBHealth summarises database health for the admin health dashboard.
type DBHealth struct {
	// SizeBytes is the on-disk size of the current database (pg_database_size).
	SizeBytes int64 `json:"size_bytes"`
	// CacheHitRatio is the shared-buffer cache hit ratio for this database
	// (blks_hit / (blks_hit + blks_read)); -1 when there has been no read
	// activity yet to compute it from.
	CacheHitRatio float64 `json:"cache_hit_ratio"`
	// Pool connection stats from the pgx pool.
	PoolTotal    int32 `json:"pool_total"`
	PoolIdle     int32 `json:"pool_idle"`
	PoolAcquired int32 `json:"pool_acquired"`
	PoolMax      int32 `json:"pool_max"`
}

// PoolStats reports pgx pool utilisation and saturation for metrics (#117),
// without a database round trip (it reads the in-memory pool stat).
type PoolStats struct {
	Total          int32
	Idle           int32
	Acquired       int32
	Max            int32
	EmptyAcquires  int64
	AcquireWaitSec float64
}

// PoolStats returns the current pgx pool statistics.
func (s *Store) PoolStats() PoolStats {
	st := s.pool.Stat()
	return PoolStats{
		Total:          st.TotalConns(),
		Idle:           st.IdleConns(),
		Acquired:       st.AcquiredConns(),
		Max:            st.MaxConns(),
		EmptyAcquires:  st.EmptyAcquireCount(),
		AcquireWaitSec: st.AcquireDuration().Seconds(),
	}
}

// DatabaseHealth returns database size, buffer cache hit ratio, and pool
// utilisation. It is cheap: pg_database_size is metadata and pg_stat_database is
// an in-memory view.
func (s *Store) DatabaseHealth(ctx context.Context) (DBHealth, error) {
	var h DBHealth

	if err := s.pool.QueryRow(ctx,
		`SELECT pg_database_size(current_database())`).Scan(&h.SizeBytes); err != nil {
		return h, fmt.Errorf("db size: %w", err)
	}

	// Cache hit ratio for the current database. When no blocks have been read
	// yet (fresh DB), the denominator is zero; report -1 to signal "unknown".
	var hit, read int64
	if err := s.pool.QueryRow(ctx, `
SELECT coalesce(blks_hit, 0), coalesce(blks_read, 0)
FROM pg_stat_database WHERE datname = current_database()`).Scan(&hit, &read); err != nil {
		return h, fmt.Errorf("cache stats: %w", err)
	}
	if hit+read > 0 {
		h.CacheHitRatio = float64(hit) / float64(hit+read)
	} else {
		h.CacheHitRatio = -1
	}

	st := s.pool.Stat()
	h.PoolTotal = st.TotalConns()
	h.PoolIdle = st.IdleConns()
	h.PoolAcquired = st.AcquiredConns()
	h.PoolMax = st.MaxConns()
	return h, nil
}
