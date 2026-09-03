package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// CreateUserInput is the data needed to create a user.
type CreateUserInput struct {
	Username     string
	PasswordHash string
	Role         string
	RateLimit    int
}

// CreateUser inserts a new user and returns it.
func (s *Store) CreateUser(ctx context.Context, in CreateUserInput) (User, error) {
	role := in.Role
	if role == "" {
		role = RoleUser
	}
	const q = `
INSERT INTO users (username, password_hash, role, rate_limit)
VALUES ($1, $2, $3, $4)
RETURNING id, username, password_hash, role, rate_limit, active, created_at, updated_at`
	var u User
	err := s.pool.QueryRow(ctx, q, in.Username, in.PasswordHash, role, in.RateLimit).Scan(
		&u.ID, &u.Username, &u.PasswordHash, &u.Role, &u.RateLimit, &u.Active,
		&u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		return User{}, fmt.Errorf("create user %q: %w", in.Username, err)
	}
	return u, nil
}

// GetUserByUsername returns the user with the given username, or ErrNotFound.
func (s *Store) GetUserByUsername(ctx context.Context, username string) (User, error) {
	return s.scanUser(ctx, `WHERE username = $1`, username)
}

// GetUserByID returns the user with the given id, or ErrNotFound.
func (s *Store) GetUserByID(ctx context.Context, id int64) (User, error) {
	return s.scanUser(ctx, `WHERE id = $1`, id)
}

func (s *Store) scanUser(ctx context.Context, where string, args ...any) (User, error) {
	q := `
SELECT id, username, password_hash, role, rate_limit, active, created_at, updated_at
FROM users ` + where
	var u User
	err := s.pool.QueryRow(ctx, q, args...).Scan(
		&u.ID, &u.Username, &u.PasswordHash, &u.Role, &u.RateLimit, &u.Active,
		&u.CreatedAt, &u.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("scan user: %w", err)
	}
	return u, nil
}

// ListUsers returns all users ordered by username.
func (s *Store) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.pool.Query(ctx, `
SELECT id, username, password_hash, role, rate_limit, active, created_at, updated_at
FROM users ORDER BY username`)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Role,
			&u.RateLimit, &u.Active, &u.CreatedAt, &u.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan user: %w", err)
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// CountUsers returns the number of users (used to detect first-run setup).
func (s *Store) CountUsers(ctx context.Context) (int64, error) {
	var n int64
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count users: %w", err)
	}
	return n, nil
}

// SetUserActive toggles a user's active flag.
func (s *Store) SetUserActive(ctx context.Context, id int64, active bool) error {
	ct, err := s.pool.Exec(ctx,
		`UPDATE users SET active = $2, updated_at = now() WHERE id = $1`, id, active)
	if err != nil {
		return fmt.Errorf("set user active: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// UpdateUserPassword sets a user's password hash.
func (s *Store) UpdateUserPassword(ctx context.Context, id int64, passwordHash string) error {
	ct, err := s.pool.Exec(ctx,
		`UPDATE users SET password_hash = $2, updated_at = now() WHERE id = $1`, id, passwordHash)
	if err != nil {
		return fmt.Errorf("update password: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteUser removes a user and cascades to their API keys.
func (s *Store) DeleteUser(ctx context.Context, id int64) error {
	ct, err := s.pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete user: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// --- API keys ---

// CreateAPIKey inserts an API key for a user.
func (s *Store) CreateAPIKey(ctx context.Context, userID int64, apiKey, label string) (APIKey, error) {
	const q = `
INSERT INTO api_keys (user_id, api_key, label)
VALUES ($1, $2, $3)
RETURNING id, user_id, api_key, label, active, last_used_at, created_at`
	var k APIKey
	err := s.pool.QueryRow(ctx, q, userID, apiKey, label).Scan(
		&k.ID, &k.UserID, &k.APIKey, &k.Label, &k.Active, &k.LastUsedAt, &k.CreatedAt)
	if err != nil {
		return APIKey{}, fmt.Errorf("create api key: %w", err)
	}
	return k, nil
}

// APIKeyUser bundles an API key with its owning user for auth lookups.
type APIKeyUser struct {
	Key  APIKey
	User User
}

// GetAPIKeyWithUser looks up an active API key and its active owner. Returns
// ErrNotFound when the key is missing, inactive, or the user is inactive.
func (s *Store) GetAPIKeyWithUser(ctx context.Context, apiKey string) (APIKeyUser, error) {
	const q = `
SELECT k.id, k.user_id, k.api_key, k.label, k.active, k.last_used_at, k.created_at,
       u.id, u.username, u.password_hash, u.role, u.rate_limit, u.active, u.created_at, u.updated_at
FROM api_keys k
JOIN users u ON u.id = k.user_id
WHERE k.api_key = $1 AND k.active = TRUE AND u.active = TRUE`
	var r APIKeyUser
	err := s.pool.QueryRow(ctx, q, apiKey).Scan(
		&r.Key.ID, &r.Key.UserID, &r.Key.APIKey, &r.Key.Label, &r.Key.Active,
		&r.Key.LastUsedAt, &r.Key.CreatedAt,
		&r.User.ID, &r.User.Username, &r.User.PasswordHash, &r.User.Role,
		&r.User.RateLimit, &r.User.Active, &r.User.CreatedAt, &r.User.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return APIKeyUser{}, ErrNotFound
	}
	if err != nil {
		return APIKeyUser{}, fmt.Errorf("get api key: %w", err)
	}
	return r, nil
}

// ListAPIKeys returns a user's API keys.
func (s *Store) ListAPIKeys(ctx context.Context, userID int64) ([]APIKey, error) {
	rows, err := s.pool.Query(ctx, `
SELECT id, user_id, api_key, label, active, last_used_at, created_at
FROM api_keys WHERE user_id = $1 ORDER BY created_at`, userID)
	if err != nil {
		return nil, fmt.Errorf("list api keys: %w", err)
	}
	defer rows.Close()
	var out []APIKey
	for rows.Next() {
		var k APIKey
		if err := rows.Scan(&k.ID, &k.UserID, &k.APIKey, &k.Label, &k.Active,
			&k.LastUsedAt, &k.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan api key: %w", err)
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// TouchAPIKey updates an API key's last-used timestamp. Best-effort; errors are
// returned but callers may ignore them.
func (s *Store) TouchAPIKey(ctx context.Context, id int64) error {
	_, err := s.pool.Exec(ctx, `UPDATE api_keys SET last_used_at = $2 WHERE id = $1`, id, time.Now())
	return err
}

// DeleteAPIKey removes an API key owned by the given user.
func (s *Store) DeleteAPIKey(ctx context.Context, userID, keyID int64) error {
	ct, err := s.pool.Exec(ctx, `DELETE FROM api_keys WHERE id = $1 AND user_id = $2`, keyID, userID)
	if err != nil {
		return fmt.Errorf("delete api key: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
