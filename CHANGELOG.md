# Changelog

All notable changes to goindex are documented here. The format is loosely based
on [Keep a Changelog](https://keepachangelog.com/), and the project tracks work
via [GitHub Issues](https://github.com/vindicare3162/NZB-Indexer/issues).

## [Unreleased]

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
