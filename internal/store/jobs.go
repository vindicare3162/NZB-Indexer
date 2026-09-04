package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Persistent pipeline jobs (#113).

// Job lifecycle states.
const (
	JobQueued      = "queued"
	JobRunning     = "running"
	JobCompleted   = "completed"
	JobFailed      = "failed"
	JobCancelled   = "cancelled"
	JobInterrupted = "interrupted"
)

// Job is a recorded pipeline operation.
type Job struct {
	ID              string     `json:"id"`
	Type            string     `json:"type"`
	State           string     `json:"state"`
	Target          string     `json:"target,omitempty"`
	ProgressCurrent int64      `json:"progress_current"`
	ProgressTotal   int64      `json:"progress_total"`
	Message         string     `json:"message,omitempty"`
	Error           string     `json:"error,omitempty"`
	CancelRequested bool       `json:"cancel_requested"`
	CreatedAt       time.Time  `json:"created_at"`
	StartedAt       *time.Time `json:"started_at,omitempty"`
	FinishedAt      *time.Time `json:"finished_at,omitempty"`
}

const jobColumns = `id, type, state, target, progress_current, progress_total, message, error, cancel_requested, created_at, started_at, finished_at`

func scanJob(row pgx.Row) (Job, error) {
	var j Job
	err := row.Scan(&j.ID, &j.Type, &j.State, &j.Target, &j.ProgressCurrent,
		&j.ProgressTotal, &j.Message, &j.Error, &j.CancelRequested,
		&j.CreatedAt, &j.StartedAt, &j.FinishedAt)
	return j, err
}

// CreateJob records a new queued job with a caller-supplied id (UUID).
func (s *Store) CreateJob(ctx context.Context, id, jobType, target string) (Job, error) {
	row := s.pool.QueryRow(ctx, `
INSERT INTO jobs (id, type, target, state)
VALUES ($1, $2, $3, 'queued')
RETURNING `+jobColumns, id, jobType, target)
	j, err := scanJob(row)
	if err != nil {
		return Job{}, fmt.Errorf("create job: %w", err)
	}
	return j, nil
}

// StartJob transitions a job to running and stamps started_at. It is a no-op
// (returns nil) if the job was already cancelled before it started.
func (s *Store) StartJob(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `
UPDATE jobs SET state = 'running', started_at = now()
WHERE id = $1 AND state = 'queued'`, id)
	if err != nil {
		return fmt.Errorf("start job: %w", err)
	}
	return nil
}

// UpdateJobProgress updates a running job's progress counters and message.
func (s *Store) UpdateJobProgress(ctx context.Context, id string, current, total int64, message string) error {
	_, err := s.pool.Exec(ctx, `
UPDATE jobs SET progress_current = $2, progress_total = $3, message = $4
WHERE id = $1`, id, current, total, message)
	if err != nil {
		return fmt.Errorf("update job progress: %w", err)
	}
	return nil
}

// FinishJob sets a terminal state (completed/failed/cancelled) with an optional
// error message and stamps finished_at.
func (s *Store) FinishJob(ctx context.Context, id, state, errMsg string) error {
	_, err := s.pool.Exec(ctx, `
UPDATE jobs SET state = $2, error = $3, finished_at = now()
WHERE id = $1`, id, state, errMsg)
	if err != nil {
		return fmt.Errorf("finish job: %w", err)
	}
	return nil
}

// RequestJobCancel flags a job for cooperative cancellation. Only queued or
// running jobs are eligible; returns ErrNotFound when the id doesn't exist or
// the job is already terminal.
func (s *Store) RequestJobCancel(ctx context.Context, id string) error {
	ct, err := s.pool.Exec(ctx, `
UPDATE jobs SET cancel_requested = TRUE
WHERE id = $1 AND state IN ('queued', 'running')`, id)
	if err != nil {
		return fmt.Errorf("request job cancel: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// IsJobCancelRequested reports whether cancellation has been requested for a
// job (polled cooperatively by the worker).
func (s *Store) IsJobCancelRequested(ctx context.Context, id string) (bool, error) {
	var requested bool
	err := s.pool.QueryRow(ctx, `SELECT cancel_requested FROM jobs WHERE id = $1`, id).Scan(&requested)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrNotFound
	}
	if err != nil {
		return false, fmt.Errorf("check job cancel: %w", err)
	}
	return requested, nil
}

// GetJob returns a single job by id.
func (s *Store) GetJob(ctx context.Context, id string) (Job, error) {
	j, err := scanJob(s.pool.QueryRow(ctx, `SELECT `+jobColumns+` FROM jobs WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, ErrNotFound
	}
	if err != nil {
		return Job{}, fmt.Errorf("get job: %w", err)
	}
	return j, nil
}

// ListJobs returns recent jobs (newest first), bounded by limit.
func (s *Store) ListJobs(ctx context.Context, limit int) ([]Job, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+jobColumns+` FROM jobs ORDER BY created_at DESC, id LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("list jobs: %w", err)
	}
	defer rows.Close()

	var out []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, fmt.Errorf("scan job: %w", err)
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// MarkInterruptedJobs marks any queued/running jobs as interrupted. Called on
// startup so jobs that were in flight when the process stopped are recovered to
// a terminal, recoverable state rather than appearing perpetually active.
// Returns the number marked.
func (s *Store) MarkInterruptedJobs(ctx context.Context) (int64, error) {
	ct, err := s.pool.Exec(ctx, `
UPDATE jobs SET state = 'interrupted', finished_at = now(),
    error = CASE WHEN error = '' THEN 'interrupted by restart' ELSE error END
WHERE state IN ('queued', 'running')`)
	if err != nil {
		return 0, fmt.Errorf("mark interrupted jobs: %w", err)
	}
	return ct.RowsAffected(), nil
}

// CleanupOldJobs deletes terminal jobs older than the given age, bounding job
// history growth. Returns the number deleted.
func (s *Store) CleanupOldJobs(ctx context.Context, olderThan time.Duration) (int64, error) {
	cutoff := time.Now().Add(-olderThan)
	ct, err := s.pool.Exec(ctx, `
DELETE FROM jobs
WHERE state IN ('completed', 'failed', 'cancelled', 'interrupted')
  AND coalesce(finished_at, created_at) < $1`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("cleanup old jobs: %w", err)
	}
	return ct.RowsAffected(), nil
}
