# goindex

A self-hosted NZB indexer. goindex connects to a Usenet (NNTP) provider, scans
newsgroup headers, assembles multi-part binaries into releases, post-processes
them (PAR2/NFO), and serves both a Newznab-compatible API (for
Sonarr/Radarr/Prowlarr) and a JSON API backing a web UI.

> Status: in active development. See the pipeline packages under `internal/`.

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

# Start the indexer (server + workers wired up in later milestones)
goindex -config config.yaml
```

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
  -p 55432:5432 postgres:16-alpine

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

The web UI is then at http://localhost:8080. Create the first admin user and an
API key from inside the container:

```sh
docker compose exec goindex goindex user add \
  -username admin -password "a-strong-password" -admin -apikey
```

Use the printed API key to add goindex to Prowlarr/Sonarr/Radarr as a Newznab
indexer pointing at `http://<host>:8080/api`.

The image is a multi-stage build: the Svelte SPA is compiled with Vite, embedded
into a static Go binary, and shipped on a minimal Alpine runtime. The container
`HEALTHCHECK` uses the binary's own `goindex healthcheck` subcommand, so no extra
tools are needed in the image.

An Unraid Community Applications template is provided at
[`docker/goindex.unraid.xml`](./docker/goindex.unraid.xml).

## Architecture

The pipeline runs as staged background workers:

```
NNTP provider
  -> pooled NNTP client        (internal/nntp)
  -> header scanner            (internal/scanner)   parts
  -> binary assembler          (internal/assembler) binaries
  -> release builder           (internal/release)   releases
  -> post-processor            (internal/postprocess) PAR2/NFO, renaming
  -> NZB generation on demand  (internal/nzb)
```

Served over a Newznab XML API (`internal/api/newznab`) and a JSON API
(`internal/api/rest`), with auth in `internal/auth`, persistence in
`internal/store`, and orchestration in `internal/worker`.
