package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// groupColumns is the shared SELECT/RETURNING column list for groups, kept in
// one place so all group queries scan the same shape.
const groupColumns = `id, name, active, last_scanned_high, backfill_low, backfill_complete, backfill_target_days, backfill_target_articles, created_at, updated_at`

// scanGroup scans a row in groupColumns order into a Group.
func scanGroup(row pgx.Row) (Group, error) {
	var g Group
	err := row.Scan(&g.ID, &g.Name, &g.Active, &g.LastScannedHigh, &g.BackfillLow,
		&g.BackfillComplete, &g.BackfillTargetDays, &g.BackfillTargetArticles,
		&g.CreatedAt, &g.UpdatedAt)
	return g, err
}

// UpsertGroup inserts a group by name or returns the existing one. The active
// flag is applied on insert; existing rows keep their current state.
func (s *Store) UpsertGroup(ctx context.Context, name string, active bool) (Group, error) {
	const q = `
INSERT INTO groups (name, active)
VALUES ($1, $2)
ON CONFLICT (name) DO UPDATE SET updated_at = now()
RETURNING ` + groupColumns
	g, err := scanGroup(s.pool.QueryRow(ctx, q, name, active))
	if err != nil {
		return Group{}, fmt.Errorf("upsert group %q: %w", name, err)
	}
	return g, nil
}

// GetGroupByName returns the group with the given name, or ErrNotFound.
func (s *Store) GetGroupByName(ctx context.Context, name string) (Group, error) {
	g, err := scanGroup(s.pool.QueryRow(ctx,
		`SELECT `+groupColumns+` FROM groups WHERE name = $1`, name))
	if errors.Is(err, pgx.ErrNoRows) {
		return Group{}, ErrNotFound
	}
	if err != nil {
		return Group{}, fmt.Errorf("get group %q: %w", name, err)
	}
	return g, nil
}

// ListGroups returns all groups ordered by name. When activeOnly is true, only
// active groups are returned.
func (s *Store) ListGroups(ctx context.Context, activeOnly bool) ([]Group, error) {
	q := `SELECT ` + groupColumns + ` FROM groups`
	if activeOnly {
		q += ` WHERE active = TRUE`
	}
	q += ` ORDER BY name`

	rows, err := s.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list groups: %w", err)
	}
	defer rows.Close()

	var out []Group
	for rows.Next() {
		g, err := scanGroup(rows)
		if err != nil {
			return nil, fmt.Errorf("scan group: %w", err)
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// SetGroupBackfillTarget sets (or clears with nil) a group's per-group backfill
// target. days and articles are independent; either may be nil to fall back to
// the global default for that dimension.
func (s *Store) SetGroupBackfillTarget(ctx context.Context, id int64, days *int, articles *int64) error {
	ct, err := s.pool.Exec(ctx, `
UPDATE groups SET backfill_target_days = $2, backfill_target_articles = $3,
                  backfill_complete = FALSE, updated_at = now()
WHERE id = $1`, id, days, articles)
	if err != nil {
		return fmt.Errorf("set backfill target: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// AnyGroupHasBackfillTarget reports whether any group has an explicit backfill
// target, so the worker can enable backfill even without a global setting.
func (s *Store) AnyGroupHasBackfillTarget(ctx context.Context) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(ctx, `
SELECT EXISTS(SELECT 1 FROM groups
              WHERE backfill_target_days IS NOT NULL OR backfill_target_articles IS NOT NULL)`).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check backfill targets: %w", err)
	}
	return exists, nil
}

// GetGroupName returns the name of the group with the given id, or ErrNotFound.
func (s *Store) GetGroupName(ctx context.Context, id int64) (string, error) {
	var name string
	err := s.pool.QueryRow(ctx, `SELECT name FROM groups WHERE id = $1`, id).Scan(&name)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("get group name %d: %w", id, err)
	}
	return name, nil
}

// SetGroupActive toggles a group's active flag.
func (s *Store) SetGroupActive(ctx context.Context, id int64, active bool) error {
	ct, err := s.pool.Exec(ctx,
		`UPDATE groups SET active = $2, updated_at = now() WHERE id = $1`, id, active)
	if err != nil {
		return fmt.Errorf("set group active: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// UpdateGroupForwardPosition advances the forward-scan watermark.
func (s *Store) UpdateGroupForwardPosition(ctx context.Context, id, high int64) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE groups SET last_scanned_high = $2, updated_at = now() WHERE id = $1`, id, high)
	if err != nil {
		return fmt.Errorf("update forward position: %w", err)
	}
	return nil
}

// UpdateGroupBackfillPosition records how far backfill has walked and whether
// it is complete.
func (s *Store) UpdateGroupBackfillPosition(ctx context.Context, id, low int64, complete bool) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE groups SET backfill_low = $2, backfill_complete = $3, updated_at = now() WHERE id = $1`,
		id, low, complete)
	if err != nil {
		return fmt.Errorf("update backfill position: %w", err)
	}
	return nil
}

// DeleteGroup removes a group and (via cascade) its parts and binaries.
func (s *Store) DeleteGroup(ctx context.Context, id int64) error {
	ct, err := s.pool.Exec(ctx, `DELETE FROM groups WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete group: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
