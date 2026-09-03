# Changelog

All notable changes to goindex are documented here. The format is loosely based
on [Keep a Changelog](https://keepachangelog.com/), and the project tracks work
via [GitHub Issues](https://github.com/vindicare3162/NZB-Indexer/issues).

## [Unreleased]

### Added
- Collection grouping for multi-file posts (#18). Multi-file Usenet posts of the
  form `[n/total] "name.partNN.rar"` (rar sets plus their PAR2) are now indexed
  as a single release containing every file's segments, instead of one bogus
  release per file. The scanner parses the `[n/total]` file counter and derives
  a stable collection key from the shared base name; the assembler folds the
  whole collection into one binary and judges completeness by files present.
  Single-file posts are unaffected. This also unblocks obfuscated-name recovery
  (#1): because the PAR2 now lands in the same release as the rar volumes,
  post-processing can read the real filenames from it.
- Real-name recovery for obfuscated releases (#1). When a release name looks
  obfuscated (random hex/base64 with no real words), post-processing now probes
  candidate segments and identifies PAR2 files by their packet magic rather than
  relying on subject filename hints, then renames the release from the recovered
  PAR2 filename. Readable names are left untouched (no wasted fetches).
- First-run setup flow (#2). On a fresh instance the web UI presents a setup
  screen to create the initial admin account, backed by public
  `GET /api/v1/setup/status` and `POST /api/v1/setup` endpoints. Setup only
  works while no users exist (guarding against privilege escalation); the CLI
  `user add` still works.
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
- Pipeline throughput at scale: neither scanning nor a large assemble backlog
  can starve post-processing (#15). The worker now runs three independent loops
  on separate goroutines and intervals: scan, assemble/build, and post-process
  (obfuscated-name recovery, NFO capture). Because post-processing has its own
  loop, name recovery keeps running even while a firehose group is being scanned
  or a huge parts backlog is being assembled. Forward scans are bounded per pass
  (`forward_max_articles`, default 1,000,000) so a firehose group yields,
  persisting its watermark to resume next pass. Intervals are configurable via
  `downstream_interval` (assemble/build, default 5m) and `postprocess_interval`
  (default 5m). Operators can also trigger an immediate post-processing pass
  from the admin UI ("Run post-processing now") or
  `POST /api/v1/admin/postprocess`; that trigger contends only with the
  post-process loop, so it runs promptly regardless of scan/assemble activity.
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
