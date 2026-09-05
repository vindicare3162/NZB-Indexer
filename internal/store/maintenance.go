package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// AnalyzeStatistics refreshes the planner statistics for the main tables (#130).
// It runs ANALYZE (non-destructive; no locks that block reads/writes) so the
// planner keeps making good choices as data grows. The table list is a fixed
// allow-list, never user input.
func (s *Store) AnalyzeStatistics(ctx context.Context) error {
	for _, t := range capacityTables {
		if _, err := s.pool.Exec(ctx, "ANALYZE "+t); err != nil {
			return fmt.Errorf("analyze %s: %w", t, err)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return nil
}

// BackupReadiness is a non-destructive backup-verification result (#130).
type BackupReadiness struct {
	// DatabaseBytes is the current on-disk size (a dump-size planning input).
	DatabaseBytes int64
	// Tables is the number of key tables that were validated as queryable.
	Tables int
	// OK reports whether every key table was reachable and countable.
	OK bool
}

// VerifyBackupReadiness performs a lightweight, read-only backup-readiness
// check (#130) without touching production data or invoking external tooling:
// it confirms the database size is readable and that each key table can be
// counted (so a logical dump would be able to read them). This validates that
// a backup could be taken; it does not perform or restore a dump. Operators
// integrate real pg_dump/verification externally; this task surfaces obvious
// pre-conditions (unreachable tables, permission loss) as an observable job.
func (s *Store) VerifyBackupReadiness(ctx context.Context) (BackupReadiness, error) {
	var br BackupReadiness
	if err := s.pool.QueryRow(ctx,
		`SELECT pg_database_size(current_database())`).Scan(&br.DatabaseBytes); err != nil {
		return br, fmt.Errorf("db size: %w", err)
	}
	for _, t := range capacityTables {
		// A cheap existence/queryability probe. LIMIT 1 avoids a full count.
		var one int
		err := s.pool.QueryRow(ctx, "SELECT 1 FROM "+t+" LIMIT 1").Scan(&one)
		// An empty table (ErrNoRows) is still reachable; only a real error fails.
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return br, fmt.Errorf("probe %s: %w", t, err)
		}
		br.Tables++
	}
	br.OK = br.Tables == len(capacityTables)
	return br, nil
}
