package store

import (
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
)

// pgDeadlockDetected is PostgreSQL's SQLSTATE for deadlock_detected.
const pgDeadlockDetected = "40P01"

// IsDeadlock reports whether err is PostgreSQL's deadlock_detected (40P01).
//
// Concurrent writers to `parts` — the assembler claiming unassembled parts into
// binaries, and re-assembly (#196) detaching them and rewriting their grouping
// — will occasionally be chosen as a deadlock victim. A deadlock is transient by
// definition: the losing transaction rolls back whole and the same work
// succeeds on a retry, so callers should retry rather than surface it as a
// pipeline failure.
func IsDeadlock(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == pgDeadlockDetected
}
