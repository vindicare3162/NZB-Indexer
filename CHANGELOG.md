# Changelog

All notable changes to goindex are documented here. The format is loosely based
on [Keep a Changelog](https://keepachangelog.com/), and the project tracks work
via [GitHub Issues](https://github.com/vindicare3162/NZB-Indexer/issues).

## [Unreleased]

### Added
- Per-group backfill targets configurable from the admin UI (#5). Each group can
  set a day cutoff and/or a per-pass article cap that overrides the global
  backfill defaults; the groups table shows the target and backfill progress.
  Backfill runs on the schedule when any group has a target.
- Newsgroup discovery in the admin UI (#4). Search the groups the provider
  carries (via NNTP `LIST ACTIVE`) with server-side caching, filtering, and
  pagination, and add groups to the index with one click. Results are ranked by
  estimated size.
- Viewable application logs in the admin UI (#7). A bounded in-memory ring
  buffer captures recent log records alongside stderr output, exposed via an
  admin-only `GET /api/v1/admin/logs` endpoint with level filtering. The admin
  page shows a live, auto-refreshing log view.

### Changed
- The assembler now drains its backlog within a single pipeline cycle (#3):
  it folds batches in a loop until nothing remains or a configurable per-run cap
  is hit, instead of one fixed batch per cycle. Large parts backlogs turn into
  releases far sooner. Context cancellation is honoured between batches.

### Added
- Configurable news servers managed from the admin UI (#6). Servers (host, port,
  TLS, credentials, max connections, priority, enabled) are stored in the
  database and editable at runtime; the NNTP pool is reconfigured live on
  change. Passwords are write-only in the API and never returned. Existing
  env/YAML NNTP config is seeded as the default server on first run. Designed to
  support multiple servers (priority-ordered), with one active at a time in v1.
- Initial goindex implementation: NNTP scanning pipeline (scan → assemble →
  release → post-process → NZB), Newznab-compatible API, JSON REST API, Svelte
  web UI, auth (users/API keys/roles/rate limiting), and Docker packaging.
- Project tracking scaffolding: issue templates, PR template, and this changelog.

### Fixed
- NNTP header scanning now uses `XOVER` (with an `OVER` fallback) instead of the
  RFC 3977 `OVER` command, which providers such as Eweka reject. Replaced the
  `chrisfarms/nntp` dependency with a direct `net/textproto` implementation.
