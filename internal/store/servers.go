package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Server is a configured NNTP news server. Password is included for internal
// use (dialing) but must never be serialised to API clients.
type Server struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	Host      string    `json:"host"`
	Port      int       `json:"port"`
	TLS       bool      `json:"tls"`
	Username  string    `json:"username"`
	Password  string    `json:"-"` // never emitted in JSON
	MaxConns  int       `json:"max_conns"`
	Priority  int       `json:"priority"`
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ServerInput is the data to create or update a server. Password is applied
// only when non-nil, so updates can omit it to leave the stored value intact.
type ServerInput struct {
	Name     string
	Host     string
	Port     int
	TLS      bool
	Username string
	Password *string
	MaxConns int
	Priority int
	Enabled  bool
}

const serverColumns = `id, name, host, port, tls, username, password, max_conns, priority, enabled, created_at, updated_at`

func scanServerRow(row pgx.Row) (Server, error) {
	var s Server
	err := row.Scan(&s.ID, &s.Name, &s.Host, &s.Port, &s.TLS, &s.Username,
		&s.Password, &s.MaxConns, &s.Priority, &s.Enabled, &s.CreatedAt, &s.UpdatedAt)
	return s, err
}

// CreateServer inserts a new server.
func (s *Store) CreateServer(ctx context.Context, in ServerInput) (Server, error) {
	pw := ""
	if in.Password != nil {
		pw = *in.Password
	}
	if in.Port <= 0 {
		in.Port = 563
	}
	if in.MaxConns <= 0 {
		in.MaxConns = 10
	}
	const q = `
INSERT INTO servers (name, host, port, tls, username, password, max_conns, priority, enabled)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
RETURNING ` + serverColumns
	srv, err := scanServerRow(s.pool.QueryRow(ctx, q,
		in.Name, in.Host, in.Port, in.TLS, in.Username, pw, in.MaxConns, in.Priority, in.Enabled))
	if err != nil {
		return Server{}, fmt.Errorf("create server: %w", err)
	}
	return srv, nil
}

// UpdateServer updates a server. When in.Password is nil the stored password is
// preserved; when non-nil (including empty string) it is replaced.
func (s *Store) UpdateServer(ctx context.Context, id int64, in ServerInput) (Server, error) {
	if in.Port <= 0 {
		in.Port = 563
	}
	if in.MaxConns <= 0 {
		in.MaxConns = 10
	}
	// COALESCE keeps the existing password when $6 is NULL.
	const q = `
UPDATE servers SET
    name = $2, host = $3, port = $4, tls = $5, username = $6,
    password = COALESCE($7, password),
    max_conns = $8, priority = $9, enabled = $10, updated_at = now()
WHERE id = $1
RETURNING ` + serverColumns
	srv, err := scanServerRow(s.pool.QueryRow(ctx, q,
		id, in.Name, in.Host, in.Port, in.TLS, in.Username, in.Password,
		in.MaxConns, in.Priority, in.Enabled))
	if errors.Is(err, pgx.ErrNoRows) {
		return Server{}, ErrNotFound
	}
	if err != nil {
		return Server{}, fmt.Errorf("update server: %w", err)
	}
	return srv, nil
}

// ListServers returns all servers ordered by priority then id.
func (s *Store) ListServers(ctx context.Context) ([]Server, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+serverColumns+` FROM servers ORDER BY priority, id`)
	if err != nil {
		return nil, fmt.Errorf("list servers: %w", err)
	}
	defer rows.Close()
	var out []Server
	for rows.Next() {
		srv, err := scanServerRow(rows)
		if err != nil {
			return nil, fmt.Errorf("scan server: %w", err)
		}
		out = append(out, srv)
	}
	return out, rows.Err()
}

// GetActiveServer returns the highest-priority enabled server, or ErrNotFound
// when none is configured.
func (s *Store) GetActiveServer(ctx context.Context) (Server, error) {
	srv, err := scanServerRow(s.pool.QueryRow(ctx,
		`SELECT `+serverColumns+` FROM servers WHERE enabled = TRUE ORDER BY priority, id LIMIT 1`))
	if errors.Is(err, pgx.ErrNoRows) {
		return Server{}, ErrNotFound
	}
	if err != nil {
		return Server{}, fmt.Errorf("get active server: %w", err)
	}
	return srv, nil
}

// DeleteServer removes a server.
func (s *Store) DeleteServer(ctx context.Context, id int64) error {
	ct, err := s.pool.Exec(ctx, `DELETE FROM servers WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete server: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// CountServers returns the number of configured servers.
func (s *Store) CountServers(ctx context.Context) (int64, error) {
	var n int64
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM servers`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count servers: %w", err)
	}
	return n, nil
}
