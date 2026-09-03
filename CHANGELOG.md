# Changelog

All notable changes to goindex are documented here. The format is loosely based
on [Keep a Changelog](https://keepachangelog.com/), and the project tracks work
via [GitHub Issues](https://github.com/vindicare3162/NZB-Indexer/issues).

## [Unreleased]

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
