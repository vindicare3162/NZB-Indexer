package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// groupColumns is the shared SELECT/RETURNING column list for groups, kept in
// one place so all group queries scan the same shape.
const groupColumns = `id, name, active, last_scanned_high, backfill_low, backfill_complete, backfill_target_days, backfill_target_articles, last_scan_at, last_scan_backfill, last_scan_articles, last_scan_parts, server_high, last_scan_error, last_scan_error_at, created_at, updated_at`

// scanGroup scans a row in groupColumns order into a Group.
func scanGroup(row pgx.Row) (Group, error) {
	var g Group
	err := row.Scan(&g.ID, &g.Name, &g.Active, &g.LastScannedHigh, &g.BackfillLow,
		&g.BackfillComplete, &g.BackfillTargetDays, &g.BackfillTargetArticles,
		&g.LastScanAt, &g.LastScanBackfill, &g.LastScanArticles, &g.LastScanParts,
		&g.ServerHigh, &g.LastScanError, &g.LastScanErrorAt,
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

// GroupFilter parameters a paginated, filtered, sorted group listing (#123),
// so the admin UI can manage hundreds/thousands of groups without loading them
// all at once.
type GroupFilter struct {
	// Search matches the group name (case-insensitive substring). Empty = all.
	Search string
	// Status filters by active state: "active", "inactive", or "" (all).
	Status string
	// ErrorsOnly, when true, returns only groups whose last scan errored.
	ErrorsOnly bool
	// Sort is the sort key: "name" (default), "lag", "last_scan", "backfill".
	Sort string
	// Desc reverses the sort order.
	Desc bool
	// Limit bounds the page size (default 50, max 500); Offset is the page start.
	Limit  int
	Offset int
}

// GroupPage is one page of a filtered group listing plus the total match count.
type GroupPage struct {
	Groups []Group `json:"groups"`
	Total  int     `json:"total"`
	Limit  int     `json:"limit"`
	Offset int     `json:"offset"`
}

// groupSortExpr maps a GroupFilter.Sort key to a SQL ORDER BY expression. name
// is always appended as a tiebreaker for stable paging.
func groupSortExpr(sort string, desc bool) string {
	dir := "ASC"
	if desc {
		dir = "DESC"
	}
	var col string
	switch sort {
	case "lag":
		// Lag = how far behind the server head the forward watermark is.
		col = "GREATEST(server_high - last_scanned_high, 0)"
	case "last_scan":
		// NULLs (never scanned) sort last regardless of direction.
		col = "last_scan_at"
	case "backfill":
		col = "backfill_low"
	default:
		col = "name"
	}
	if col == "name" {
		return "name " + dir
	}
	return col + " " + dir + " NULLS LAST, name ASC"
}

// ListGroupsPage returns a filtered, sorted, paginated page of groups plus the
// total number of groups matching the filter (#123).
func (s *Store) ListGroupsPage(ctx context.Context, f GroupFilter) (GroupPage, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	offset := f.Offset
	if offset < 0 {
		offset = 0
	}

	// Build the shared WHERE clause and args.
	var conds []string
	var args []any
	add := func(cond string, val any) {
		args = append(args, val)
		conds = append(conds, fmt.Sprintf(cond, len(args)))
	}
	if f.Search != "" {
		add("name ILIKE '%%' || $%d || '%%'", f.Search)
	}
	switch f.Status {
	case "active":
		conds = append(conds, "active = TRUE")
	case "inactive":
		conds = append(conds, "active = FALSE")
	}
	if f.ErrorsOnly {
		conds = append(conds, "last_scan_error <> ''")
	}
	where := ""
	if len(conds) > 0 {
		where = " WHERE " + strings.Join(conds, " AND ")
	}

	// Total count for the filter.
	var total int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM groups`+where, args...).Scan(&total); err != nil {
		return GroupPage{}, fmt.Errorf("count groups: %w", err)
	}

	// The page. limit/offset are the last two positional args.
	args = append(args, limit, offset)
	q := `SELECT ` + groupColumns + ` FROM groups` + where +
		` ORDER BY ` + groupSortExpr(f.Sort, f.Desc) +
		fmt.Sprintf(" LIMIT $%d OFFSET $%d", len(args)-1, len(args))

	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return GroupPage{}, fmt.Errorf("list groups page: %w", err)
	}
	defer rows.Close()

	out := make([]Group, 0, limit)
	for rows.Next() {
		g, err := scanGroup(rows)
		if err != nil {
			return GroupPage{}, fmt.Errorf("scan group: %w", err)
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return GroupPage{}, err
	}
	return GroupPage{Groups: out, Total: total, Limit: limit, Offset: offset}, nil
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

// GroupScanOutcome captures the result of one scan/backfill pass for a group,
// recorded per group for progress/error reporting (#114).
type GroupScanOutcome struct {
	Backfill bool
	Articles int64
	Parts    int64
	// ServerHigh is the server's high-water article number observed this pass
	// (0 when it could not be read); persisted only when > 0 so a failed pass
	// that never read the bounds keeps the last known value.
	ServerHigh int64
	// Err is the pass error message ('' on success).
	Err string
}

// RecordGroupScan records the outcome of the most recent scan/backfill pass for
// a group: when it ran, how much it pulled, the observed server head, and any
// error. On success it clears the previous error; on failure it records the
// error and its timestamp while leaving the counters/watermarks (already
// persisted incrementally by the scanner) intact. server_high is only updated
// when a positive value was observed, so a failure that never read the group
// bounds does not zero out the last known head.
func (s *Store) RecordGroupScan(ctx context.Context, id int64, o GroupScanOutcome) error {
	const q = `
UPDATE groups SET
    last_scan_at       = now(),
    last_scan_backfill = $2,
    last_scan_articles = $3,
    last_scan_parts    = $4,
    server_high        = CASE WHEN $5::bigint > 0 THEN $5::bigint ELSE server_high END,
    last_scan_error    = $6,
    last_scan_error_at = CASE WHEN $6 <> '' THEN now() ELSE NULL END,
    updated_at         = now()
WHERE id = $1`
	if _, err := s.pool.Exec(ctx, q, id, o.Backfill, o.Articles, o.Parts, o.ServerHigh, o.Err); err != nil {
		return fmt.Errorf("record group scan: %w", err)
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
