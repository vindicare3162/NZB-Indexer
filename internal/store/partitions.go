package store

import (
	"context"
	"fmt"
	"regexp"
	"time"
)

// partitionBoundRE extracts the FROM/TO literals from a RANGE partition bound
// expression like: FOR VALUES FROM ('2026-06-01 00:00:00+00') TO ('...').
var partitionBoundRE = regexp.MustCompile(`FROM \('([^']+)'\) TO \('([^']+)'\)`)

// Time-partitioning of the high-volume `parts` table (#119).
//
// At large scale, deleting hundreds of millions of raw article rows with
// ordinary DELETEs creates long transactions, dead tuples, and index bloat.
// Native declarative RANGE partitioning of `parts` by ingest month
// (created_at) lets retention drop an entire expired month as an instant
// metadata-only operation instead of a table-wide DELETE, and keeps autovacuum
// per-partition.
//
// Partitioning is an operator/ops rollout decision (it changes the natural key
// to include the partition column and requires migrating existing data), so it
// is NOT applied automatically by the migration chain. See
// docs/parts-partitioning.md for the resumable conversion procedure. The
// functions here manage partitions and work whether or not `parts` is currently
// partitioned:
//
//   - PartsIsPartitioned reports whether `parts` is a partitioned table.
//   - EnsurePartsPartitions creates the current and next N monthly partitions
//     (idempotent), so new rows always route into a predictable partition.
//   - ListPartsPartitions reports the existing monthly partitions and bounds.
//   - DropExpiredPartsPartitions drops whole partitions that lie entirely
//     before the retention cutoff (retention-aware, no table-wide DELETE).
//   - CheckPartsPartitionCoverage returns an actionable error when the
//     partition for "now" (or the near future) is missing, for monitoring.
//
// When `parts` is not partitioned, Ensure/Drop/Check are safe no-ops (or report
// "not partitioned") so the same code runs on both layouts.

// PartsPartition describes one monthly range partition of `parts`.
type PartsPartition struct {
	// Name is the partition table name, e.g. "parts_2026_02".
	Name string `json:"name"`
	// From/To are the half-open range bounds [From, To) on created_at.
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
}

// PartsIsPartitioned reports whether the `parts` relation is a partitioned
// table (RANGE-partitioned by created_at when set up per the rollout doc).
func (s *Store) PartsIsPartitioned(ctx context.Context) (bool, error) {
	var partitioned bool
	err := s.pool.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1 FROM pg_partitioned_table pt
    JOIN pg_class c ON c.oid = pt.partrelid
    WHERE c.relname = 'parts'
)`).Scan(&partitioned)
	if err != nil {
		return false, fmt.Errorf("check parts partitioned: %w", err)
	}
	return partitioned, nil
}

// monthStart returns the first instant of t's month in UTC.
func monthStart(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// partitionName renders the monthly partition name for a month-start time.
func partitionName(monthStart time.Time) string {
	return fmt.Sprintf("parts_%04d_%02d", monthStart.Year(), int(monthStart.Month()))
}

// EnsurePartsPartitions creates the partition covering `now` plus the next
// `future` months (idempotent). It also ensures the current month exists. When
// `parts` is not partitioned it is a no-op returning 0. Returns how many
// partitions were newly created.
func (s *Store) EnsurePartsPartitions(ctx context.Context, now time.Time, future int) (int, error) {
	partitioned, err := s.PartsIsPartitioned(ctx)
	if err != nil {
		return 0, err
	}
	if !partitioned {
		return 0, nil
	}
	if future < 0 {
		future = 0
	}

	created := 0
	start := monthStart(now)
	for i := 0; i <= future; i++ {
		from := start.AddDate(0, i, 0)
		to := from.AddDate(0, 1, 0)
		name := partitionName(from)

		existed, err := s.partitionExists(ctx, name)
		if err != nil {
			return created, fmt.Errorf("check partition %s: %w", name, err)
		}
		if existed {
			continue
		}
		// Idempotent create; IF NOT EXISTS guards against a concurrent creator.
		q := fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s PARTITION OF parts
FOR VALUES FROM ('%s') TO ('%s')`,
			name, from.Format("2006-01-02"), to.Format("2006-01-02"))
		if _, err := s.pool.Exec(ctx, q); err != nil {
			return created, fmt.Errorf("create partition %s: %w", name, err)
		}
		created++
	}
	return created, nil
}

func (s *Store) partitionExists(ctx context.Context, name string) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_class WHERE relname = $1 AND relkind = 'r')`, name).Scan(&exists)
	return exists, err
}

// ListPartsPartitions returns the monthly partitions of `parts` with their
// range bounds, ordered oldest first. Empty when `parts` is not partitioned.
func (s *Store) ListPartsPartitions(ctx context.Context) ([]PartsPartition, error) {
	rows, err := s.pool.Query(ctx, `
SELECT c.relname,
       pg_get_expr(c.relpartbound, c.oid) AS bound
FROM pg_inherits i
JOIN pg_class parent ON parent.oid = i.inhparent
JOIN pg_class c ON c.oid = i.inhrelid
WHERE parent.relname = 'parts'
ORDER BY c.relname`)
	if err != nil {
		return nil, fmt.Errorf("list partitions: %w", err)
	}
	defer rows.Close()

	var out []PartsPartition
	for rows.Next() {
		var name, bound string
		if err := rows.Scan(&name, &bound); err != nil {
			return nil, fmt.Errorf("scan partition: %w", err)
		}
		p := PartsPartition{Name: name}
		// Parse "FOR VALUES FROM ('2026-02-01 00:00:00+00') TO ('2026-03-01 ...')".
		if from, to, ok := parsePartitionBound(bound); ok {
			p.From, p.To = from, to
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// parsePartitionBound extracts the FROM/TO timestamps from a RANGE partition
// bound expression. Returns ok=false when it can't be parsed (e.g. DEFAULT).
func parsePartitionBound(bound string) (from, to time.Time, ok bool) {
	// Bound looks like: FOR VALUES FROM ('2026-02-01 00:00:00+00') TO ('2026-03-01 00:00:00+00')
	m := partitionBoundRE.FindStringSubmatch(bound)
	if len(m) != 3 {
		return time.Time{}, time.Time{}, false
	}
	layouts := []string{"2006-01-02 15:04:05-07", "2006-01-02 15:04:05Z07", "2006-01-02 15:04:05", "2006-01-02"}
	parse := func(s string) (time.Time, bool) {
		for _, l := range layouts {
			if t, err := time.Parse(l, s); err == nil {
				return t.UTC(), true
			}
		}
		return time.Time{}, false
	}
	f, okf := parse(m[1])
	t, okt := parse(m[2])
	if !okf || !okt {
		return time.Time{}, time.Time{}, false
	}
	return f, t, true
}

// DropExpiredPartsPartitions drops every partition whose entire range lies
// strictly before the cutoff (created_at < cutoff), reclaiming its storage as a
// fast metadata-only DROP rather than a table-wide DELETE. It only drops
// partitions fully older than the cutoff (a partition straddling the cutoff is
// kept). Returns the names dropped. No-op (nil) when `parts` is not
// partitioned.
//
// Safety: this is a coarse, time-only expiry. Callers that must honour the
// row-level retention reconstructability invariants (see retention.go) should
// only drop partitions old enough that every row in them is unquestionably
// expired, or run the row-wise prune instead.
func (s *Store) DropExpiredPartsPartitions(ctx context.Context, cutoff time.Time) ([]string, error) {
	parts, err := s.ListPartsPartitions(ctx)
	if err != nil {
		return nil, err
	}
	var dropped []string
	for _, p := range parts {
		if p.To.IsZero() {
			continue // unparseable/default partition: never auto-drop
		}
		// Drop only if the partition's upper bound is at or before the cutoff,
		// i.e. the whole partition is older than the retention boundary.
		if !p.To.After(cutoff) {
			if _, err := s.pool.Exec(ctx, fmt.Sprintf("DROP TABLE IF EXISTS %s", p.Name)); err != nil {
				return dropped, fmt.Errorf("drop partition %s: %w", p.Name, err)
			}
			dropped = append(dropped, p.Name)
		}
	}
	return dropped, nil
}

// CheckPartsPartitionCoverage returns an actionable error when `parts` is
// partitioned but has no partition covering `now` (so an insert at `now` would
// fail with "no partition of relation"). It is intended for monitoring/health
// so a missing future partition is surfaced before ingestion breaks. Returns
// nil when coverage is present or `parts` is not partitioned.
func (s *Store) CheckPartsPartitionCoverage(ctx context.Context, now time.Time) error {
	partitioned, err := s.PartsIsPartitioned(ctx)
	if err != nil {
		return err
	}
	if !partitioned {
		return nil
	}
	parts, err := s.ListPartsPartitions(ctx)
	if err != nil {
		return err
	}
	now = now.UTC()
	for _, p := range parts {
		if p.From.IsZero() || p.To.IsZero() {
			continue
		}
		if !now.Before(p.From) && now.Before(p.To) {
			return nil // covered
		}
	}
	return fmt.Errorf("no parts partition covers %s; create it (e.g. EnsurePartsPartitions) before ingestion fails", now.Format("2006-01"))
}
