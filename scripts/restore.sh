#!/usr/bin/env bash
#
# restore.sh — restore a goindex PostgreSQL backup (created by backup.sh) into
# the running Docker Compose stack.
#
# WARNING: this is DESTRUCTIVE. It drops and recreates the objects present in
# the dump (pg_restore --clean --if-exists), overwriting current data in the
# target database. Stop the goindex app first to avoid writes during restore.
#
# Usage:
#   scripts/restore.sh <dump_file>
#
# Environment (all optional; defaults match docker-compose.yml):
#   DB_SERVICE   Compose service name for Postgres (default: db)
#   DB_NAME      Database name (default: goindex)
#   DB_USER      Database user (default: goindex)
#   COMPOSE      docker compose command (default: "docker compose")
#   FORCE        Set to 1 to skip the confirmation prompt (for automation)
#
# Example:
#   scripts/restore.sh backups/goindex-goindex-20260903-120000.dump

set -euo pipefail

if [ $# -lt 1 ]; then
  echo "Usage: $0 <dump_file>" >&2
  exit 2
fi

DUMP_FILE="$1"
DB_SERVICE="${DB_SERVICE:-db}"
DB_NAME="${DB_NAME:-goindex}"
DB_USER="${DB_USER:-goindex}"
COMPOSE="${COMPOSE:-docker compose}"
FORCE="${FORCE:-0}"

if [ ! -f "${DUMP_FILE}" ]; then
  echo "ERROR: dump file not found: ${DUMP_FILE}" >&2
  exit 1
fi

# Verify the dump is readable before touching the database.
if ! ${COMPOSE} exec -T "${DB_SERVICE}" pg_restore -l < "${DUMP_FILE}" > /dev/null 2>&1; then
  echo "ERROR: '${DUMP_FILE}' is not a valid custom-format dump" >&2
  exit 1
fi

echo "About to restore '${DUMP_FILE}' into database '${DB_NAME}' (service '${DB_SERVICE}')."
echo "This OVERWRITES current data. Make sure the goindex app is stopped."
if [ "${FORCE}" != "1" ]; then
  printf "Type 'yes' to continue: "
  read -r reply
  if [ "${reply}" != "yes" ]; then
    echo "Aborted."
    exit 1
  fi
fi

echo "Restoring…"
# --clean --if-exists: drop existing objects first (safe if absent).
# --no-owner/--no-privileges: match the portable backup. Errors during --clean
# on a fresh DB are expected and non-fatal, so we do not use --exit-on-error.
${COMPOSE} exec -T "${DB_SERVICE}" \
  pg_restore -U "${DB_USER}" -d "${DB_NAME}" --clean --if-exists --no-owner --no-privileges \
  < "${DUMP_FILE}"

echo "Restore complete. Start the goindex app to resume."
