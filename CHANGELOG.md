# Changelog

All notable changes to goindex are documented here. The format is loosely based
on [Keep a Changelog](https://keepachangelog.com/), and the project tracks work
via [GitHub Issues](https://github.com/vindicare3162/NZB-Indexer/issues).

## [Unreleased]

### Changed
- Binary assembly is now set-based, cutting database round trips (#116).
  `AssembleBinaries` previously issued three statements (aggregate, upsert, link)
  per grouping inside a Go loop — roughly `2 + 3N` round trips per batch. It now
  folds an entire batch with two single-statement CTE pipelines (one for
  multi-file collections, one for single-file posts): each aggregates the
  selected unassigned groupings, bulk-upserts the binaries with the same
  additive-accumulation and completeness semantics via `ON CONFLICT DO UPDATE`,
  and bulk-links the parts by joining on the grouping key — a small constant
  number of round trips regardless of how many groupings a batch contains.
  Behaviour is unchanged: out-of-order and across-scan accumulation,
  single-article completeness, and collection distinct-file completeness all
  hold.
- Release search now has shareable URL state and a richer UX (#124). The query,
  category, page, page size, and include-obfuscated toggle are encoded in the
  URL hash (e.g. `#/?q=movie&cat=2000&page=2`), so a search is deep-linkable and
  browser back/forward restores it. Typing runs a debounced live search that
  updates the URL in place (no history spam, bounded API traffic), while
  submitting or paging adds a history entry. Results use page-based navigation
  that works with the capped/approximate totals from #120, each row gains a
  Copy-NZB-URL button, and loading/empty/error/stale states are clearer. The
  URL-state serialize/parse logic lives in a unit-tested module
  (`web/src/lib/searchstate.js`); the hash router now exposes `path` + `query`
  with `pushQuery`/`replaceQuery` and back/forward support.

### Added
- Per-group scan progress and error reporting (#114). Each group now records the
  outcome of its most recent scan/backfill pass — when it ran, whether it was a
  forward scan or backfill, how many articles/parts it pulled, the server's
  observed high-water article number, and the last error (with timestamp) if the
  pass failed. A new migration (0016) adds these columns to `groups`, the worker
  records the outcome after each per-group pass (best-effort, non-blocking), and
  the fields flow through the existing `GET /admin/groups` and `/admin/overview`
  responses. The admin Groups table gains Lag (how far behind the server head)
  and Last scan columns plus an error badge; the formatting logic lives in a
  unit-tested module (`web/src/lib/groupscan.js`). A failed pass preserves the
  last known server head rather than zeroing it.
- Persistent pipeline jobs with IDs, progress, cancellation, and history (#113).
  Manual scan, backfill, and post-processing triggers now create a durable job
  record (UUID id) tracked through a lifecycle (`queued` → `running` →
  `completed`/`failed`/`cancelled`), persisted in a new `jobs` table (migration
  0015). The trigger endpoints (`POST /admin/scan`, `/admin/backfill`,
  `/admin/postprocess`) return `{"status":"accepted","job_id":...}`, and three
  new admin endpoints expose the history: `GET /admin/jobs` (recent, newest
  first), `GET /admin/jobs/{id}`, and `POST /admin/jobs/{id}/cancel`.
  Cancellation is cooperative: it sets a `cancel_requested` flag the worker polls
  and also cancels the in-flight pass's context when the job is running locally.
  Jobs interrupted by a restart are reconciled to an `interrupted` state on
  startup, and a background loop prunes job history older than 7 days. A new
  Jobs panel in the admin UI lists jobs with state, progress, timing, and a
  Cancel action, polling on the existing refresh cadence.
- Automated Newznab client-contract tests (#136) hardening compatibility with
  Prowlarr, Sonarr, and Radarr. The suite parses the actual XML and asserts the
  exact shapes clients depend on: caps (limits, per-search-type
  `supportedParams`, nested parent/subcategory structure), search/tvsearch/movie
  (RSS `version`/namespace, `newznab:response` offset+total, item
  `title`/`guid`/`link`/`pubDate` RFC1123Z/`enclosure` url+length+`application/x-nzb`,
  `newznab:attr` size/grabs/category), details, and get (NZB content type +
  `Content-Disposition`, grab increment). It also covers pagination passthrough
  and limit clamping, category-list/parent expansion, empty results, malformed
  requests and Newznab error codes (200/202/300), a full client workflow
  sequence, and a guard that every advertised search param is one the handler
  actually resolves. Combined with the existing API-key / rate-limit contract
  (401/429, `Retry-After`, `X-RateLimit-*`) this locks the client-facing surface
  against regressions. Documented in `docs/newznab-compat.md`. No behavior
  changes — the existing surface already met the contract.
- NNTP provider health checks, failover, and circuit breaking (#128). The
  pipeline now routes NNTP work across all enabled DB-managed servers by
  priority: the highest-priority healthy provider serves each request, and on a
  connection or authentication failure that provider's circuit breaker trips and
  work fails over to the next healthy provider, so indexing continues through a
  fallback instead of failing. Errors are classified (connection, auth,
  protocol, retention) — protocol/retention responses (the server answered) are
  returned without failover, while repeated connection failures (or any auth
  failure) open the circuit to prevent retry storms. Opened circuits are probed
  after a cooldown (half-open) and restored on success. Per-provider circuit
  state, failure counts, last error, and pool utilisation are exposed in the
  admin health report (`usenet.providers`) and SPA, and an open circuit raises a
  health check (warn, or error when every provider is down). Configurable via
  `GOINDEX_NNTP_CIRCUIT_FAILURE_THRESHOLD` / `GOINDEX_NNTP_CIRCUIT_COOLDOWN`.
  Editing servers rebuilds the failover rotation live (existing servers keep
  their circuit state). Race-enabled tests cover failover, recovery,
  simultaneous failures, and protocol-vs-connection handling.
- Opt-in time-partitioning strategy for the high-volume `parts` table (#119).
  At large scale, native declarative RANGE partitioning by ingest month
  (`created_at`) lets retention drop an entire expired month as an instant
  metadata-only `DROP TABLE` instead of a table-wide `DELETE`, and keeps
  autovacuum per-partition. New store partition management (safe no-ops when
  `parts` is not partitioned): `EnsurePartsPartitions` (idempotently create the
  current + N future monthly partitions so new rows always route into a
  predictable partition), `ListPartsPartitions`, `DropExpiredPartsPartitions`
  (drop only partitions whose whole range is older than the retention cutoff),
  and `CheckPartsPartitionCoverage` (actionable error for monitoring when the
  partition for "now" is missing, before ingestion would fail). Partitioning is
  operator-driven (it changes the natural key and migrates existing rows), so it
  is not applied automatically; `docs/parts-partitioning.md` documents the
  partition key, application compatibility, and a resumable, low-downtime
  conversion procedure for existing installs. Integration tests cover partition
  detection, routing, coverage/missing-partition detection, idempotent creation,
  expiry (dropping only fully-expired partitions while retaining a straddling
  one), and listing/bounds parsing.

### Changed
- Article/part ingestion now uses PostgreSQL COPY for bulk loading (#115).
  Instead of one `INSERT` per article, a scan batch is loaded via `COPY` into a
  per-transaction `TEMP` staging table and folded into `parts` with a single
  set-based `INSERT ... SELECT DISTINCT ON (group_id, article_number) ...
  ON CONFLICT DO NOTHING`. This materially improves high-volume header-ingestion
  throughput while preserving idempotency: rows conflicting on the natural key —
  already stored or duplicated within the batch — are skipped, and the accurate
  count of newly inserted rows is still returned. The whole load is one
  transaction, so a failed or cancelled batch inserts nothing (watermark-safe),
  and memory stays bounded by the caller's batch size (staging table is
  `ON COMMIT DROP`). Constraints and indexes are unchanged. Tests cover
  idempotent re-scans, in-batch duplicates, cancellation/rollback, and empty
  batches; a benchmark exercises 10k/100k batches.
- Forward scans are now prioritized over historical backfill (#112). Previously
  each scan cycle ran a full forward pass and then a full backfill pass
  sequentially, so a large backfill could delay indexing of newly posted
  content. Scanning now drains backfill one group at a time and yields to any
  forward pass that becomes due (ticker or manual trigger), so forward work is
  never delayed by more than the current backfill group, while backfill still
  makes progress when forward demand is low. Forward and backfill continue to
  share a single scan goroutine, so no group is ever scanned forward and
  backward concurrently and per-group watermarks stay correct. Scheduling is
  implemented by a decoupled, deterministically-tested scanner (`scanScheduler`)
  covering long backfill, overdue forward work, cancellation, and fairness.

### Added
- Efficient release-search pagination (#120). Release search now supports
  **keyset (cursor) pagination** for the JSON API: passing the `next_cursor`
  token from a response fetches the next page by predicate
  (`(posted_at,id) < cursor`) instead of a growing `OFFSET`, so deep pages no
  longer scan and discard all preceding rows. Exact counts are now **capped**:
  a broad search counts at most a bounded number of matches and returns
  `approximate: true` with a capped `total` rather than scanning the whole
  catalogue on every request (configurable via `SearchFilter.CountCap`; a
  negative value forces an exact count). The search response adds `next_cursor`,
  `has_more`, and `approximate`; the SPA uses `has_more` so navigation works
  with capped totals and shows `N+` for approximate results. Newznab
  `limit`/`offset`/`total` behaviour is unchanged for client compatibility.
  Ordering is stable across concurrent inserts and duplicate timestamps (id
  tiebreaker). Store tests cover keyset/offset equivalence, capped/exact counts,
  and a query-plan assertion that the keyset page carries no OFFSET.
- Configurable raw-part retention with a dry-run mode (#118). Raw article rows
  for released items that are fully post-processed and reconstructable from
  durable NZB segments (from #105) can now be pruned after a retention window,
  so storage tracks intended retention rather than installation lifetime.
  Retention is opt-in and conservative by default (`GOINDEX_RETENTION_ENABLED`,
  `GOINDEX_RETENTION_DAYS`, plus interval/batch-size/max-batches knobs). A part
  is only prunable when its binary is released, its release is `pp_status='done'`
  with non-empty durable segments, and it is older than the window; unassigned
  backlog, incomplete/unreleased binaries, releases still pending/failed
  post-processing, and non-reconstructable releases are always retained. New
  admin endpoints `GET /api/v1/admin/retention/preview` (dry-run report of
  candidate parts, estimated bytes, oldest/newest, and retention reasons) and
  `POST /api/v1/admin/retention/prune` (bounded, resumable, cancellable batched
  deletion), surfaced in the admin UI, plus an optional background retention
  loop. Integration tests cover the safety predicate, batched/cancellable
  deletion, and confirm pruned releases still generate NZBs from durable
  segments.

### Changed
- NNTP connection capacity is now safely reconfigurable at runtime (#111).
  Previously the pool's concurrency ceiling was fixed at construction, so an
  admin change to a server's max-connections updated connection parameters but
  silently kept the old capacity until a restart. The pool now uses a resizable
  counting semaphore: growing the limit immediately wakes waiting workers, and
  shrinking stops granting new slots beyond the new limit while letting in-flight
  operations finish — so the number of concurrent connections never exceeds
  either the old or the new limit during the transition and no operation is
  interrupted. Editing the active server applies the new ceiling live, and the
  effective capacity (and in-use count) is logged and reported in the health
  status. Covered by race-enabled tests for growing, shrinking, and resizing
  under active load, plus pool-shutdown safety.

### Added
- PostgreSQL and NNTP resource budgeting (#117). Pipeline concurrency is now
  sized against **both** the effective NNTP capacity and the PostgreSQL pool
  instead of the NNTP pool alone. Startup reserves database headroom for the
  HTTP API and admin control plane (`GOINDEX_DB_RESERVED_CONNS`; default
  auto-derives ≈¼ of the pool, `[1,4]`, always leaving ≥1 for the pipeline) and
  clamps the combined scan + post-process worker footprint to the remaining
  "DB pipeline budget", so pipeline load cannot starve searches/admin requests.
  Explicit overrides (`GOINDEX_SCAN_CONCURRENCY`) are honoured but log a warning
  when they overcommit the budget. New Prometheus metrics expose pool
  utilisation and saturation: `goindex_nntp_pool_*`, `goindex_db_pool_*`
  (including `goindex_db_pool_empty_acquires_total` and
  `goindex_db_pool_acquire_wait_seconds_total`), and the static
  `goindex_db_reserved_api_connections` / `goindex_db_pipeline_budget_connections`.
  A constrained-pool test proves work queues on connection acquisition rather
  than deadlocking or starving the API. `docs/postgres-tuning.md` explains how
  provider limits, pool size, worker counts, and the reservation interact.
- Aggregated admin overview endpoint `GET /api/v1/admin/overview` (#110). It
  returns the whole dashboard — health, worker status, pipeline statistics,
  groups, servers, users, schedule, and bounded recent logs — in one
  admin-authenticated request with a `generated_at` timestamp. Each subsystem is
  gathered best-effort: a failing section is reported under an `errors` map
  while the rest of the payload still loads, rather than failing the whole
  response. Credentials are never included (server passwords are redacted to a
  `has_password` flag; user password hashes are never serialised). The admin
  page now loads via this single request instead of many independent calls,
  coalesces overlapping refreshes, and discards stale responses; the detailed
  per-resource endpoints remain for targeted refreshes.

### Changed
- Per-group backfill targets are now configured with an accessible, validated
  inline form instead of browser `prompt()` dialogs (#109). The editor explains
  the impact of the limits and the blank/zero/explicit semantics (blank = use
  global default, 0 = unlimited, positive = explicit limit), validates the days
  and article-limit fields before submission, lets each override be cleared
  independently, and keeps the form open with a visible error on save failure so
  the operator can retry. Validation and payload logic live in a unit-tested
  module (`web/src/lib/backfill.js`), and the web package now runs Vitest via
  `npm test`.

### Added
- Normalized external release identifiers (#108). A new `release_identifiers`
  table (migration 0014) stores per-release `(source, identifier)` pairs for
  supported providers (imdb, tvdb, tmdb), normalized to canonical form (IMDb
  `tt<digits>`; TVDB/TMDB decimal digits) and de-duplicated per release. The
  REST release detail now returns an `identifiers` array (surfaced in the SPA
  as linked External-ID tags), and both the internal and Newznab search filters
  can match releases by identifier. Newznab `imdbid`/`tvdbid`/`tmdbid` params
  are now matched against these stored identifiers instead of being folded into
  the release-name text query, and the capabilities response advertises them on
  the relevant search types. Identifiers are populated by metadata enrichment
  (a later change); until then, id searches match only releases that already
  carry the identifier.
- API-key authentication is now cached in-process with a short TTL, and
  `last_used_at` writes are throttled (#107). Repeated Newznab polling within
  the TTL avoids the per-request key/user lookup and per-request last-used
  write (one lookup per TTL window, one write per throttle interval per key).
  Deleting a key or user invalidates the cache immediately; otherwise entries
  expire within the TTL. Cache hit/miss/eviction/size are exported as Prometheus
  metrics (`goindex_apikey_cache_*`). No external cache (Redis) is used.
- Release search is backed by a PostgreSQL **pg_trgm GIN index** (#106) so the
  tokenized `LIKE '%token%'` substring search scales as the catalog grows.
  Migration 0013 creates the `pg_trgm` extension and
  `idx_releases_search_name_trgm`; the planner uses a bitmap index scan for
  substring predicates (verified via `EXPLAIN`). Search semantics are unchanged.
  `docs/postgres-tuning.md` documents the strategy and measured plan.
- Durable release segments make NZB generation independent of raw parts (#105,
  the prerequisite for raw-part retention). At build time a release snapshots
  its ordered segments into a new `releases.segments` column (migration 0012),
  and NZB generation reads from there, falling back to the raw-parts join only
  for legacy releases. A release's NZB now generates correctly after its backing
  parts are deleted. `POST /api/v1/admin/segments/backfill` (and a "Backfill NZB
  segments" admin button) snapshots segments for pre-existing legacy releases
  and reports how many were repaired vs unresolvable.

### Fixed
- Scan and post-processing concurrency are now sized from the **effective**
  NNTP connection limit — the one the live pool was actually built with (the
  active DB-managed server when present) — rather than the startup config,
  which could differ and mis-size the worker pools (#104). Startup logs and the
  admin health report now expose the effective capacity and derived scan/pp
  worker limits.

### Added
- Bounded **parallel group scanning** (#102, implementing the design in #100).
  A scan/backfill pass now processes several groups concurrently via a worker
  pool instead of one at a time, so large group counts (50-500) no longer
  serialise. Sized by `GOINDEX_SCAN_CONCURRENCY` (0 = auto, derived from the
  NNTP pool size; 1 = sequential/previous behaviour); real parallelism is still
  capped by `nntp.max_conns`, which scanning shares with post-processing. The
  "Current tasks" panel now shows overall `completed/total` plus the in-flight
  groups.
- "Current tasks" now shows scan/backfill **progress** — the group currently
  being scanned and its position in the list, e.g. `Scanning:
  alt.binaries.teevee (12/500)` (#98). Groups are scanned sequentially, so this
  makes it obvious how far a pass has got (and whether it's falling behind) at
  scale. Exposed via `scan_progress` in `/api/v1/admin/status`.
- "Current tasks" section on the Admin page (under System health) showing which
  pipeline stages are running right now — scan/backfill, assemble, build,
  post-process, enrich (#96). The worker now tracks per-stage activity as a set
  (loops run concurrently, so several can be active at once) and exposes it via
  `active_stages` in `/api/v1/admin/status`; the panel shows "Idle" when nothing
  is running and refreshes with the admin page's 5s poll.

### Changed
- Access-log entries now use a concise action label as the message (e.g.
  `backfill`, `postprocess`, `scan`, `stats`, `search`, `nzb download`) instead
  of a uniform `http request`, so the admin Logs view is readable at a glance
  (#94). The structured attributes (method/path/status/duration/bytes/remote/
  user) are unchanged; unmapped paths still log `http request`.

### Added
- Admin health dashboard (#84). A new `GET /api/v1/admin/health` endpoint and a
  "System health" panel on the Admin page surface process stats (goroutines,
  heap, uptime), database health (size, buffer cache-hit ratio, pool
  utilisation), Usenet connection pool state, and a set of "potential issue"
  checks (DB reachability, no news server, no groups, low cache-hit ratio,
  permanently-failed post-processing, default JWT secret). Overall status is the
  worst check. Host CPU/disk/network remain the domain of node_exporter/the
  Prometheus metrics (see `docs/monitoring.md`).
- Optional release metadata enrichment (#82). When enabled
  (`GOINDEX_METADATA_ENABLED=true`), a background loop matches TV releases to
  shows via the keyless TVMaze provider and stores title, year, season/episode,
  a cover image, and an overview (migration 0011, `release_metadata`). The
  release detail page shows the cover and overview, and the JSON release detail
  gains a `metadata` object. Providers are behind an interface; the feature is
  off by default and degrades gracefully (disabled or provider errors leave
  releases unchanged). Configurable via `config.example.yaml` /
  `GOINDEX_METADATA_PROVIDER` / `GOINDEX_METADATA_INTERVAL`.
- Prometheus metrics at `GET /metrics` and a Grafana dashboard (#78). Exposes
  HTTP request counters/latency (with bounded `route` labels), pipeline-depth
  gauges (parts/binaries/releases, releases-by-pp-status, failed-exhausted) and
  worker activity counters (articles pulled, releases created/renamed, cycles),
  plus the standard Go/process collectors. A ready-to-import dashboard lives at
  `docs/grafana/goindex-dashboard.json`; see `docs/monitoring.md` for scrape
  config. The endpoint is unauthenticated by convention and exposes no secrets.
- Database backup and recovery scripts (#76): `scripts/backup.sh` produces a
  compressed, restore-friendly `pg_dump` (custom format) from the running
  Compose stack with optional retention pruning, and `scripts/restore.sh`
  restores a dump (`pg_restore --clean --if-exists`) with a confirmation
  prompt. The README documents manual use, cron scheduling, retention, and the
  destructive-restore caveat. The dump/restore round-trip is verified against
  PostgreSQL 18.

### Fixed
- Obfuscated releases no longer leak into default search after post-processing
  (#92). When a release's PAR2 carried an internal filename that was itself
  random hex, post-processing "recovered" that junk name and cleared the
  `obfuscated` flag, so the release surfaced in default (non-obfuscated) search.
  Post-processing now rejects a recovered name that is itself obfuscated,
  leaving the release flagged and hidden by default.
- Loose-file collections — a post of many individual files (e.g. `index.html`,
  `script.js`, `.course_id`, plus a PAR2 set) with a shared `[n/total]` counter
  — are now grouped into a single release instead of one release per file
  (#90). Collection detection keys on the release title preceding the file
  counter (false-merge-safe), so the whole post (content + parity) collapses
  into one correctly-named release; classic archive sets and single-file posts
  are unaffected.
- Multi-file collections posted with a title-prefixed file counter
  (`Release.Name [64/65] - "file.par2" ...`) are no longer fragmented into a
  separate release per file (#88). The leading-file-counter parser was anchored
  to the start of the subject (missing the title-prefixed style), and a guard
  wrongly rejected collections whose file-count equalled the per-file segment
  count; both are fixed, so such posts group into a single release.
- Compose: mount the database volume at `/var/lib/postgresql` (not
  `/var/lib/postgresql/data`) so `postgres:18-alpine` starts — the PG18 image
  changed its data-directory layout and refused to start with the old mount,
  leaving `db` unhealthy on a fresh `docker compose up` (#86, regression from
  the PG18 bump in #74).

### Changed
- Upgraded the bundled database image from PostgreSQL 16 to **18**
  (`postgres:18-alpine`) for the newer major's performance work, notably the
  PostgreSQL 18 asynchronous I/O subsystem that speeds up sequential scans,
  bitmap heap scans and vacuums — all heavily used over the large `parts` and
  `releases` tables (#74). The full test suite passes against PG 18.6.
  **Upgrade note:** a new PostgreSQL major will not start on a data directory
  initialised by an older major. Fresh deployments are unaffected; existing
  `goindex-db` volumes must be migrated with a `pg_dump`/`pg_restore` (or
  `pg_upgrade`) before switching the image.

### Docs
- Added `docs/parallel-scanning-design.md` (#100): a scoping/design record for
  bounded parallel group scanning (throughput at 50-500 groups) — a
  worker-pool refactor of the sequential scan loop mirroring post-processing,
  gated by a new `Scan.Concurrency` knob sharing the NNTP `MaxConns` budget,
  with a multi-group progress model. Implementation tracked separately.
- Added `docs/postgres-tuning.md` (#80): a database growth model and PostgreSQL
  tuning guide — the `parts` table dominates, so the highest-value levers are
  autovacuum/analyze tuning on `parts` (with concrete `ALTER TABLE` settings)
  and, when storage becomes a constraint, a retention policy on released
  binaries' parts (partitioning at very large scale). Also covers connection
  pool sizing and server-memory settings for a containerised node.
- Added `docs/redis-caching-analysis.md` (#72): a decision record recommending
  in-process caching (categories/caps + API-key auth) now and deferring Redis
  to the multi-instance scaling boundary, where a shared rate limiter/cache
  actually requires it.

### Added
- Bulk newsgroup add/enable (#70). A new admin action adds/enables many groups
  at once with an optional backfill window (e.g. 7 days), via
  `POST /api/v1/admin/groups/bulk` and a "Bulk add" panel on the Admin page
  (paste newline/comma-separated names). It's idempotent — existing groups are
  reported as such — and the backfill target is applied to each group.
- The pipeline schedule is now editable at runtime from the admin UI (#68),
  building on the env/YAML config from #52. A new Schedule panel sets the scan,
  assemble, build and post-process intervals; changes apply to the running
  worker immediately (each loop resets its ticker, no restart) and persist
  across restarts in a new `settings` table (migration 0010). Persisted values
  take precedence over env/YAML defaults on startup. New admin endpoints
  `GET/PUT /api/v1/admin/schedule` accept Go duration strings (e.g. `5m`).
- Structured access logging for all HTTP requests (#66). A middleware wrapping
  the whole server emits one record per request with `method`, `path`,
  `status`, `duration_ms`, `bytes`, `remote`, and `user` (when authenticated).
  5xx responses log at WARN; the `/health` and `/ready` probes log at DEBUG so
  frequent orchestrator polling doesn't drown out real traffic.
- Readiness probe `GET /api/v1/ready` (#64), distinct from the liveness probe
  `GET /api/v1/health`. `/ready` pings the database (2s timeout) and returns 503
  when it is unreachable, so orchestrators stop routing traffic to an instance
  whose DB is down; `/health` remains an unconditional liveness check.
- Categorization improvements (#62): the release search results table now shows
  each release's category name; the JSON search API accepts a comma-separated
  `cat` list (matching the Newznab handler, so clients requesting several
  categories are honoured); and the name classifier gained coverage for
  Audiobooks, TV/Sport, TV/Documentary, Comics and Movies/Foreign, which were
  seeded categories the classifier never previously emitted.
- The full pipeline schedule is now configurable via environment variables
  (#52). In addition to `GOINDEX_SCAN_INTERVAL`, the assemble, release-build and
  post-processing cadences and the forward-scan cap are settable without a YAML
  file: `GOINDEX_SCAN_DOWNSTREAM_INTERVAL`, `GOINDEX_SCAN_BUILD_INTERVAL`,
  `GOINDEX_SCAN_POSTPROCESS_INTERVAL`, `GOINDEX_SCAN_FORWARD_MAX_ARTICLES`.
  `config.example.yaml` documents the corresponding YAML keys.
- Obfuscated releases are excluded from search by default (#54). Releases whose
  name is still random hex/base64 (post-processing couldn't recover a real name)
  are flagged `obfuscated` at build time and cleared when a real name is
  recovered. Search (JSON + Newznab) hides them by default so real releases are
  no longer buried; the web UI has an "Include obfuscated" toggle and the JSON
  API accepts `include_obfuscated=1`. The obfuscation heuristic now lives in one
  place (`release.IsObfuscated`, shared with post-processing).
- Rate-limit headers on the Newznab API (#51). Authenticated responses now
  carry `X-RateLimit-Limit` / `X-RateLimit-Remaining` / `X-RateLimit-Reset`, and
  a 429 (rate-limited) response includes a `Retry-After` header (seconds until
  the window resets) so clients like Prowlarr/Sonarr back off instead of
  retrying immediately.
- Per-group release breakdown in the stats view (#47). `GET /api/v1/admin/stats`
  now includes a `groups` array (per group: name, total releases, releases
  pending post-processing), and the admin Pipeline-depth panel shows a
  "Releases by group" table. Computed from the releases table (a single grouped
  scan), so it stays cheap; raw per-group part backlog is intentionally excluded
  as too expensive to poll.
- "Retry failed post-processing" admin action (#45). Releases whose PAR2/NFO
  fetch exhausted the retry budget stay `failed`; an operator can now requeue
  all of them (reset to `pending`, `pp_attempts=0`) from the admin UI or
  `POST /api/v1/admin/postprocess/retry-failed`, which also kicks a
  post-processing pass. Useful after a temporary provider problem is resolved.
- Failed / exhausted-retry post-processing counts in the stats view (#40). The
  admin Pipeline-depth panel now shows how many releases are pending, done,
  failed-but-awaiting-retry, and failed with retries exhausted (permanently
  stuck). `GET /api/v1/admin/stats` gained `releases_failed_exhausted`
  (`pp_status='failed' AND pp_attempts >= max`), computed via the retry index so
  it stays cheap.
- Pipeline backlog/health stats (#26). A new admin endpoint
  `GET /api/v1/admin/stats` reports current pipeline depth: estimated total and
  unassembled parts (the assembler backlog), binary counts (total/complete/
  complete-but-unreleased), and releases by post-processing status. The admin
  page shows these so an operator can see at a glance whether the pipeline is
  keeping up. Parts totals use planner estimates so the endpoint stays cheap
  even with tens of millions of parts.
- Resolution-aware categorization (#23). Movies and TV releases are now filed
  under their SD/HD/UHD subcategory (Movies 2030/2040/2045, TV 5030/5040/5045)
  based on resolution tags in the name (2160p/4k -> UHD, 1080p/720p -> HD,
  480p/DVDRip/SD -> SD; HD when unspecified), so Newznab clients can filter by
  quality. Releases are also re-categorized when post-processing recovers a real
  name, so an obfuscated release that started as "Other" gets the correct
  category once its name is known.
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

### Fixed
- Admin menu no longer disappears after a page reload (#42). The web UI kept
  only the auth token across reloads, so the in-memory role was empty until the
  next login and the Admin nav vanished. On startup the UI now restores the
  username/role from `GET /api/v1/me` when a token exists, so admin status
  persists across reloads (and an invalid token cleanly logs out).
- Newznab caps no longer advertise unsupported id search params (#55). As a
  header-only indexer, goindex can't resolve `rid`/`tvdbid`/`imdbid` to
  releases, so advertising them led clients to prefer id searches that return
  nothing. Caps now advertise only what works: `q,cat,season,ep` for TV and
  `q,cat` for movies, so clients fall back to text/episode searches.
- Parts with non-UTF-8 bytes in their subject/poster are now stored (#49).
  NNTP overview lines are byte streams and subjects/posters routinely carry
  Latin-1/CP437 bytes; PostgreSQL `TEXT` rejected them, failing the insert (and
  potentially the whole batch) and silently dropping parts. Overview fields are
  now sanitized to valid UTF-8 (invalid sequences replaced) at the NNTP parse
  boundary before reaching the store.
- Newznab external-id searches behave correctly (#43). `imdbid` is now
  normalized (bare or `tt`-prefixed) and added as a search token for
  `t=movie`. Critically, an id-based search (`imdbid`/`tvdbid`/`rid`/...) that a
  header-only indexer can't resolve now returns an empty feed instead of the
  entire catalogue, so clients no longer treat every release as a match. Bare
  browse (`t=search` with no query) still returns recent releases.
- Search now matches multi-word and tvsearch season/episode queries (#37).
  Search matched the whole query as one contiguous substring, so a Sonarr
  episode search (`q=<series>&season=S&ep=E`, sent to the store as e.g.
  `saving s03e10`) never matched a real release name like
  `saving grace s03e10 hdtv xvid`. The query is now tokenized and every token
  must appear in the name (order-independent AND), so episode searches work.
- Post-processing retries transient fetch failures instead of silently giving
  up (#35). Previously a failed PAR2/NFO article fetch (timeout, connection
  blip) was treated the same as "nothing to recover": the release was marked
  `done` and never retried, silently losing recoverable names/NFOs on a busy
  provider. A needed fetch that errors now marks the release retryable and it is
  re-queued on later passes, bounded to a few attempts (tracked by a new
  `pp_attempts` column) so genuinely-unrecoverable releases don't retry forever.
- PAR2-recovered names no longer keep a volume suffix (#31). When recovering a
  name from PAR2, a part filename like `Show.S03E10.HDTV.XviD.part1.rar` was
  reduced only to `Show.S03E10.HDTV.XviD.part1` (the `.part1` was left behind),
  producing a worse name than the original. Volume suffixes (`.partNN`,
  `.volNN+NN`, `.rNN`) are now stripped along with the archive extension so a
  recovery set collapses to its clean base name.
- Single-article posts are now released (#28). When an article subject has no
  yEnc segment counter, the scanner records `total_parts = 0`; the assembler
  treated such binaries as incomplete forever, so single-article files (small
  files, NZBs, images, single-file obfuscated posts) never became releases and
  were eventually aged out and deleted. A binary with no declared total and at
  least one collected part is now treated as a complete single-file binary.
  (On a live instance this was the majority of "incomplete" binaries.)
- PAR2 filename recovery now works (#21). The PAR2 packet type field was read at
  the wrong byte offset (40 instead of the spec's 48), so File Description
  packets were never recognised and post-processing recovered no names or NFOs
  against a real provider. Reading the type at the correct offset restores name
  and NFO recovery. (The test fixture shared the same offset mistake, which had
  masked the bug; it and a dedicated regression test now pin the spec offset.)
- A stalled article-body fetch no longer blocks post-processing (#22). Each
  fetch is bounded by a per-fetch timeout (`FetchTimeout`, default 30s) applied
  as a socket read deadline, so one hung article can no longer stall the whole
  post-processing loop.

### Changed
- Post-processing now processes releases concurrently (#33). A pass fans pending
  releases out to a bounded worker pool (default ~half the NNTP connection
  budget) instead of handling them one at a time, so a slow PAR2/NFO fetch on
  one release no longer stalls the rest and the pending backlog drains far
  faster. Per-release semantics and result counts are unchanged.
- Release-building runs in its own loop (#18 follow-up). Previously assembly and
  release-building shared a loop (assemble-until-drained, then build), so a
  large parts backlog could keep the assembler busy and complete binaries never
  became releases. Building now runs on its own goroutine/interval
  (`build_interval`, default 2m), independent of assembly. The assembler's
  grouping queries also dropped an unnecessary `ORDER BY`, and a partial index
  was added so grouping stays cheap at scale instead of sequentially scanning
  the whole unassigned backlog each pass.
- Collection detection is stricter (#18 follow-up): a single multi-segment file
  whose subject repeats the same counter in the leading and trailing positions
  (e.g. `[1/445] "blob" (1/445)`), or an obfuscated blob with no archive/parity
  extension, is no longer mis-detected as a multi-file collection.
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
