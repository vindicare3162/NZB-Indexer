package store

import (
	"context"
	"fmt"
)

// capacityTables is the fixed set of tables surfaced in the capacity report.
// It is a hardcoded allow-list (never user input) so the size queries are safe.
var capacityTables = []string{
	"parts", "binaries", "releases", "release_files", "release_metadata",
	"release_identifiers", "groups", "jobs",
}

// CapacityTableNames returns the fixed set of tables surfaced in capacity and
// maintenance operations (a copy, so callers cannot mutate the allow-list).
func CapacityTableNames() []string {
	return append([]string(nil), capacityTables...)
}

// TableSize is the on-disk footprint of one table (#131).
type TableSize struct {
	Name       string `json:"name"`
	TotalBytes int64  `json:"total_bytes"` // table + indexes + toast
	TableBytes int64  `json:"table_bytes"` // heap only
	IndexBytes int64  `json:"index_bytes"` // indexes only
	Rows       int64  `json:"rows"`        // planner estimate (reltuples)
}

// GroupStorageRank ranks one group by retained raw-part storage (#131).
type GroupStorageRank struct {
	Name       string `json:"name"`
	Bytes      int64  `json:"bytes"`
	Parts      int64  `json:"parts"`
}

// GroupRateRank ranks one group by observed ingest rate (#131).
type GroupRateRank struct {
	Name          string  `json:"name"`
	ArtsPerSecond float64 `json:"arts_per_second"`
}

// CapacityStats is the measured basis for growth/capacity planning (#131). It
// carries current sizes plus observed ingest rate; projections are computed
// separately by the pure ProjectCapacity function so the math is unit-testable.
type CapacityStats struct {
	// DatabaseBytes is the total on-disk database size.
	DatabaseBytes int64 `json:"database_bytes"`
	// Tables is the per-table breakdown (largest first).
	Tables []TableSize `json:"tables"`
	// PartsBytes is the total retained raw-part storage (the dominant, and
	// retention-controllable, growth driver).
	PartsBytes int64 `json:"parts_bytes"`
	// ObservedArtsPerSecond is the summed per-group throughput EMA across active
	// groups (#127), i.e. the current observed ingest rate.
	ObservedArtsPerSecond float64 `json:"observed_arts_per_second"`
	// BytesPerArticle is the mean stored bytes per part article, used to convert
	// article rates into byte growth (0 when no parts yet).
	BytesPerArticle float64 `json:"bytes_per_article"`
	// TopGroupsByStorage / TopGroupsByRate are per-group rankings.
	TopGroupsByStorage []GroupStorageRank `json:"top_groups_by_storage"`
	TopGroupsByRate    []GroupRateRank    `json:"top_groups_by_rate"`
}

// CapacityStats gathers the current sizes and observed rates for capacity
// planning (#131). Size queries read pg_catalog metadata (cheap, no secrets);
// rate/storage rankings read the small groups table and a bounded parts
// aggregate. topN bounds the per-group rankings (<=0 uses 10).
func (s *Store) CapacityStats(ctx context.Context, topN int) (CapacityStats, error) {
	if topN <= 0 {
		topN = 10
	}
	var cs CapacityStats

	if err := s.pool.QueryRow(ctx,
		`SELECT pg_database_size(current_database())`).Scan(&cs.DatabaseBytes); err != nil {
		return cs, fmt.Errorf("db size: %w", err)
	}

	// Per-table sizes from the catalog. The table list is a fixed allow-list.
	for _, t := range capacityTables {
		var ts TableSize
		ts.Name = t
		err := s.pool.QueryRow(ctx, `
SELECT
    coalesce(pg_total_relation_size(c.oid), 0),
    coalesce(pg_table_size(c.oid), 0),
    coalesce(pg_indexes_size(c.oid), 0),
    coalesce(c.reltuples, 0)::bigint
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.relname = $1 AND n.nspname = 'public'`, t).Scan(
			&ts.TotalBytes, &ts.TableBytes, &ts.IndexBytes, &ts.Rows)
		if err != nil {
			// A missing table (e.g. optional) is skipped, not fatal.
			continue
		}
		cs.Tables = append(cs.Tables, ts)
	}
	// Largest table first.
	for i := 0; i < len(cs.Tables); i++ {
		for j := i + 1; j < len(cs.Tables); j++ {
			if cs.Tables[j].TotalBytes > cs.Tables[i].TotalBytes {
				cs.Tables[i], cs.Tables[j] = cs.Tables[j], cs.Tables[i]
			}
		}
	}

	// Total retained raw-part storage and mean bytes/article.
	var partsBytes, partsCount int64
	if err := s.pool.QueryRow(ctx,
		`SELECT coalesce(sum(bytes), 0), count(*) FROM parts`).Scan(&partsBytes, &partsCount); err != nil {
		return cs, fmt.Errorf("parts storage: %w", err)
	}
	cs.PartsBytes = partsBytes
	if partsCount > 0 {
		cs.BytesPerArticle = float64(partsBytes) / float64(partsCount)
	}

	// Observed ingest rate: sum of per-group throughput EMA across active groups.
	if err := s.pool.QueryRow(ctx,
		`SELECT coalesce(sum(throughput_arts_per_sec), 0) FROM groups WHERE active = TRUE`).
		Scan(&cs.ObservedArtsPerSecond); err != nil {
		return cs, fmt.Errorf("observed rate: %w", err)
	}

	// Top groups by retained storage.
	rows, err := s.pool.Query(ctx, `
SELECT g.name, coalesce(sum(p.bytes), 0) AS bytes, count(p.id) AS parts
FROM groups g
JOIN parts p ON p.group_id = g.id
GROUP BY g.id, g.name
ORDER BY bytes DESC
LIMIT $1`, topN)
	if err != nil {
		return cs, fmt.Errorf("top groups by storage: %w", err)
	}
	for rows.Next() {
		var r GroupStorageRank
		if err := rows.Scan(&r.Name, &r.Bytes, &r.Parts); err != nil {
			rows.Close()
			return cs, fmt.Errorf("scan storage rank: %w", err)
		}
		cs.TopGroupsByStorage = append(cs.TopGroupsByStorage, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return cs, err
	}

	// Top groups by observed rate.
	rrows, err := s.pool.Query(ctx, `
SELECT name, throughput_arts_per_sec
FROM groups
WHERE active = TRUE AND throughput_arts_per_sec > 0
ORDER BY throughput_arts_per_sec DESC
LIMIT $1`, topN)
	if err != nil {
		return cs, fmt.Errorf("top groups by rate: %w", err)
	}
	for rrows.Next() {
		var r GroupRateRank
		if err := rrows.Scan(&r.Name, &r.ArtsPerSecond); err != nil {
			rrows.Close()
			return cs, fmt.Errorf("scan rate rank: %w", err)
		}
		cs.TopGroupsByRate = append(cs.TopGroupsByRate, r)
	}
	rrows.Close()
	return cs, rrows.Err()
}

// CapacityProjection is the forecast for one horizon (#131).
type CapacityProjection struct {
	Days int `json:"days"`
	// GrowthBytes is the projected additional part storage over the horizon at
	// the observed ingest rate.
	GrowthBytes int64 `json:"growth_bytes"`
	// ProjectedDatabaseBytes is the current database size plus GrowthBytes (a
	// simple upper bound assuming no pruning).
	ProjectedDatabaseBytes int64 `json:"projected_database_bytes"`
	// RetainedBytes is the steady-state part storage under the retention window
	// (bytes produced within RetentionDays), 0 when retention is disabled.
	RetainedBytes int64 `json:"retained_bytes"`
}

// CapacityForecast bundles the observed basis and the horizon projections
// (#131), with the measurement assumptions made explicit for the operator.
type CapacityForecast struct {
	// DailyArticles / DailyBytes are the observed ingest rate scaled to a day.
	DailyArticles float64 `json:"daily_articles"`
	DailyBytes    int64   `json:"daily_bytes"`
	// RetentionDays is the retention window used for the steady-state estimate
	// (0 = retention disabled / unbounded growth).
	RetentionDays int `json:"retention_days"`
	// Projections are the per-horizon forecasts (30/90/365 days by default).
	Projections []CapacityProjection `json:"projections"`
	// Assumptions documents how the forecast was derived so its confidence is
	// visible.
	Assumptions string `json:"assumptions"`
}

// ProjectCapacity turns the observed basis into a growth/retention forecast
// (#131). It is a pure function (no DB) so the math is unit-testable against
// known synthetic rates. dbBytes is the current database size; artsPerSecond is
// the observed ingest rate; bytesPerArticle converts articles to bytes;
// retentionDays is the configured raw-part retention window (0 = disabled);
// horizons are the forecast windows in days (nil uses 30/90/365).
func ProjectCapacity(dbBytes int64, artsPerSecond, bytesPerArticle float64, retentionDays int, horizons []int) CapacityForecast {
	if len(horizons) == 0 {
		horizons = []int{30, 90, 365}
	}
	dailyArticles := artsPerSecond * 86400
	dailyBytes := dailyArticles * bytesPerArticle

	f := CapacityForecast{
		DailyArticles: dailyArticles,
		DailyBytes:    int64(dailyBytes),
		RetentionDays: retentionDays,
		Assumptions: "Growth is projected from the summed per-group throughput EMA (recent " +
			"successful passes) times the mean stored bytes per article. Projected " +
			"database size assumes no pruning; retained bytes assume a steady state " +
			"where only parts within the retention window are kept. Backup/WAL " +
			"overhead is not included.",
	}
	for _, days := range horizons {
		growth := dailyBytes * float64(days)
		proj := CapacityProjection{
			Days:                   days,
			GrowthBytes:            int64(growth),
			ProjectedDatabaseBytes: dbBytes + int64(growth),
		}
		if retentionDays > 0 {
			proj.RetainedBytes = int64(dailyBytes * float64(retentionDays))
		}
		f.Projections = append(f.Projections, proj)
	}
	return f
}
