package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// ListCategories returns all categories ordered by id (parents sort before
// their children given the Newznab numbering convention).
func (s *Store) ListCategories(ctx context.Context) ([]Category, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, parent_id, name, description FROM categories ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list categories: %w", err)
	}
	defer rows.Close()

	var out []Category
	for rows.Next() {
		var c Category
		if err := rows.Scan(&c.ID, &c.ParentID, &c.Name, &c.Description); err != nil {
			return nil, fmt.Errorf("scan category: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetCategory returns a single category by id, or ErrNotFound.
func (s *Store) GetCategory(ctx context.Context, id int) (Category, error) {
	var c Category
	err := s.pool.QueryRow(ctx,
		`SELECT id, parent_id, name, description FROM categories WHERE id = $1`, id).
		Scan(&c.ID, &c.ParentID, &c.Name, &c.Description)
	if errors.Is(err, pgx.ErrNoRows) {
		return Category{}, ErrNotFound
	}
	if err != nil {
		return Category{}, fmt.Errorf("get category %d: %w", id, err)
	}
	return c, nil
}

// CategoryExists reports whether a category id is present.
func (s *Store) CategoryExists(ctx context.Context, id int) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM categories WHERE id = $1)`, id).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("category exists %d: %w", id, err)
	}
	return exists, nil
}
