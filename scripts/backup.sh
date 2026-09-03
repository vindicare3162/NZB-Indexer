#!/usr/bin/env bash
#
# backup.sh — create a compressed, restore-friendly backup of the goindex
# PostgreSQL database from the running Docker Compose stack.
#
# It runs pg_dump in custom format (-Fc) inside the "db" service and writes a
# timestamped dump to the backups directory on the host. Custom format is
# compressed and supports selective/parallel restore via pg_restore.
#
# Usage:
#   scripts/backup.sh [output_dir]
#
# Environment (all optional; defaults match docker-compose.yml):
#   BACKUP_DIR        Where to write dumps (default: ./backups, or $1)
#   DB_SERVICE        Compose service name for Postgres (default: db)
#   DB_NAME           Database name (default: goindex)
#   DB_USER           Database user (default: goindex)
#   RETENTION_DAYS    Delete dumps older than this many days (default: 0 = keep all)
#   COMPOSE           docker compose command (default: "docker compose")
#
# Example (keep 14 days of daily backups):
#   RETENTION_DAYS=14 scripts/backup.sh /var/backups/goindex

set -euo pipefail

BACKUP_DIR="${1:-${BACKUP_DIR:-./backups}}"
DB_SERVICE="${DB_SERVICE:-db}"
DB_NAME="${DB_NAME:-goindex}"
DB_USER="${DB_USER:-goindex}"
RETENTION_DAYS="${RETENTION_DAYS:-0}"
COMPOSE="${COMPOSE:-docker compose}"

timestamp="$(date +%Y%m%d-%H%M%S)"
outfile="${BACKUP_DIR}/goindex-${DB_NAME}-${timestamp}.dump"

mkdir -p "${BACKUP_DIR}"

echo "Backing up database '${DB_NAME}' from service '${DB_SERVICE}' -> ${outfile}"

# -Fc: custom (compressed) format. Stream to the host file via stdout so we do
# not need a writable path inside the container. --no-owner/--no-privileges keep
# the dump portable across roles.
${COMPOSE} exec -T "${DB_SERVICE}" \
  pg_dump -U "${DB_USER}" -d "${DB_NAME}" -Fc --no-owner --no-privileges \
  > "${outfile}"

# Basic integrity check: a valid custom-format dump lists its table of contents.
if ! ${COMPOSE} exec -T "${DB_SERVICE}" pg_restore -l < "${outfile}" > /dev/null 2>&1; then
  echo "ERROR: backup verification failed (pg_restore -l could not read ${outfile})" >&2
  rm -f "${outfile}"
  exit 1
fi

size="$(du -h "${outfile}" | cut -f1)"
echo "Backup complete: ${outfile} (${size})"

# Optional retention: prune old dumps.
if [ "${RETENTION_DAYS}" -gt 0 ]; then
  echo "Pruning backups older than ${RETENTION_DAYS} day(s) in ${BACKUP_DIR}"
  find "${BACKUP_DIR}" -maxdepth 1 -type f -name "goindex-${DB_NAME}-*.dump" \
    -mtime "+${RETENTION_DAYS}" -print -delete
fi
