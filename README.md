# goindex

A self-hosted NZB indexer. goindex connects to a Usenet (NNTP) provider, scans
newsgroup headers, assembles multi-part binaries and multi-file collections
into releases, post-processes them (PAR2 name recovery, NFO capture), and serves
both a Newznab-compatible API (for Sonarr/Radarr/Prowlarr) and a JSON API
backing a web UI.

## Features

- **Newznab + JSON APIs** for Sonarr/Radarr/Prowlarr and the built-in web UI.
- **First-run setup in the browser**: a fresh instance prompts you to create the
  initial admin account; no CLI step required.
- **Configurable news servers** managed from the admin UI (multi-server).
- **Newsgroup discovery**: search the groups your provider carries and add them
  with one click.
- **Per-group backfill targets**: index older posts back to N days or M articles
  per group, overriding the global default.
- **Decoupled pipeline**: scan, assemble, release-build, and post-process run as
  independent background loops, so a slow stage can't starve the others.
- **Collection grouping**: multi-file posts (`[n/total] "name.partNN.rar"` sets
  plus their PAR2) become a single release.
- **Real-name recovery** for obfuscated releases via PAR2, with NFO capture.
- **Resolution-aware categorization** (Movies/TV SD/HD/UHD).
- **Operational visibility**: live application logs, pipeline status, and a
  pipeline-depth/backlog view in the admin UI.

## Requirements

- Go 1.24+ (developed against 1.27)
- PostgreSQL 14+
- A Usenet provider account with NNTP access

## Configuration

Configuration loads from defaults, then an optional YAML file, then
`GOINDEX_`-prefixed environment variables (env wins). See
[`config.example.yaml`](./config.example.yaml) for all options.

## Building

```sh
go build ./...
go build -o goindex ./cmd/goindex
```

## Running

```sh
# Apply database migrations
goindex migrate

# Smoke-test the NNTP connection
goindex nntp-test -group alt.binaries.example

# Start the indexer (HTTP server + background pipeline workers)
goindex -config config.yaml
```

On first start, open the web UI and create the initial admin account through the
setup screen. From there, add your news server(s), discover and enable groups,
and configure backfill — all from the admin page.

## Testing

Unit tests run with no external dependencies:

```sh
go test ./...
```

Integration tests (store, scanner, assembler) require a PostgreSQL instance.
They are **skipped** unless `GOINDEX_TEST_DSN` is set. Because the integration
tests share one database and each resets the schema, run them with package
parallelism disabled (`-p 1`) so packages don't clobber each other:

```sh
# Start a disposable Postgres
docker run --rm -d --name goindex-test-pg \
  -e POSTGRES_USER=goindex -e POSTGRES_PASSWORD=goindex -e POSTGRES_DB=goindex_test \
  -p 55432:5432 postgres:18-alpine

# Run the full suite against it
GOINDEX_TEST_DSN="postgres://goindex:goindex@localhost:55432/goindex_test?sslmode=disable" \
  go test ./... -p 1 -count=1
```

## Docker

The fastest way to run goindex plus PostgreSQL:

```sh
# Set at minimum a JWT secret, DB password, and your NNTP provider details.
export GOINDEX_JWT_SECRET="$(openssl rand -hex 32)"
export GOINDEX_DB_PASSWORD="a-strong-password"
export GOINDEX_NNTP_HOST="news.your-provider.com"
export GOINDEX_NNTP_USERNAME="you"
export GOINDEX_NNTP_PASSWORD="secret"

docker compose up -d
```

The web UI is then at http://localhost:8080 (override the published port with
`GOINDEX_PORT`). On first visit, the setup screen prompts you to create the
initial admin account. After logging in, add your news server, discover/enable
groups, and generate an API key from the admin page.

> The CLI can still create a user if you prefer:
> `docker compose exec goindex goindex user add -username admin -password "..." -admin -apikey`

Use the API key to add goindex to Prowlarr/Sonarr/Radarr as a Newznab indexer
pointing at `http://<host>:8080/api`.

The image is a multi-stage build: the Svelte SPA is compiled with Vite, embedded
into a static Go binary, and shipped on a minimal Alpine runtime. The container
`HEALTHCHECK` uses the binary's own `goindex healthcheck` subcommand, so no extra
tools are needed in the image.

An Unraid Community Applications template is provided at
[`docker/goindex.unraid.xml`](./docker/goindex.unraid.xml).

## Backup and recovery

Two helper scripts back up and restore the PostgreSQL database from the running
Compose stack. Backups use `pg_dump` custom format (compressed and
restore-friendly).

Create a backup (written to `./backups` by default):

```sh
scripts/backup.sh
# or choose a directory and keep 14 days of dumps:
RETENTION_DAYS=14 scripts/backup.sh /var/backups/goindex
```

Schedule daily backups with cron (adjust paths):

```cron
0 3 * * * cd /opt/goindex && RETENTION_DAYS=14 scripts/backup.sh /var/backups/goindex >> /var/log/goindex-backup.log 2>&1
```

Restore a backup (**destructive** — overwrites the current database; stop the
app first):

```sh
docker compose stop goindex
scripts/restore.sh backups/goindex-goindex-20260101-030000.dump
docker compose start goindex
```

Notes:
- Dumps contain all indexed data and are not encrypted; store them somewhere
  appropriate and secure the destination directory.
- Restore drops and recreates the objects in the dump (`pg_restore --clean
  --if-exists`); run it against a stopped app to avoid concurrent writes.
- Both scripts honour `DB_SERVICE`, `DB_NAME`, `DB_USER`, and `COMPOSE`
  overrides if your setup differs from the defaults.

## Monitoring

goindex exposes Prometheus metrics at `GET /metrics` (HTTP request/latency
counters plus pipeline-depth and worker gauges). Point Prometheus at it and
import the bundled Grafana dashboard. See
[`docs/monitoring.md`](./docs/monitoring.md) for the metric list, scrape config,
and dashboard import steps.

## Architecture

The pipeline stages are:

```
NNTP provider
  -> pooled NNTP client        (internal/nntp)
  -> header scanner            (internal/scanner)     parts
  -> binary assembler          (internal/assembler)   binaries / collections
  -> release builder           (internal/release)     releases + categorization
  -> post-processor            (internal/postprocess) PAR2 name recovery, NFO
  -> NZB generation on demand  (internal/nzb)
```

The worker (`internal/worker`) runs the scan, assemble, release-build, and
post-process stages as **independent loops on their own intervals**, so a
long-running scan or a large assemble backlog cannot starve release-building or
name recovery.

Served over a Newznab XML API (`internal/api/newznab`) and a JSON API
(`internal/api/rest`), with auth in `internal/auth` and persistence in
`internal/store`.
