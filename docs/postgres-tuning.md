# Database growth and PostgreSQL tuning

_Status: analysis / operations guide. Tracking issue: #80._

This documents how goindex's database grows, where load concentrates, and how to
tune PostgreSQL for it. It is grounded in the current schema (migrations
0001–0010) and the pipeline's access patterns.

## Growth model

| Table | Row meaning | Relative size | Growth driver |
|-------|-------------|---------------|---------------|
| `parts` | One Usenet article header | **Dominant** (10s of millions) | Every forward scan / backfill pass |
| `binaries` | A collected multi-part file set | Small | Assembler folds parts into binaries |
| `releases` | A searchable, named item | Small | Builder promotes complete binaries |
| `release_files` | Per-file segments for NZB rebuild | Moderate | Post-processing (PAR2/NFO recovery) |
| `categories`, `groups`, `users`, `api_keys`, `settings` | Config / reference | Tiny | Rarely |

`parts` dominates everything: storage, write throughput, autovacuum work, and
index size. The pipeline reflects this — `PipelineStatistics` deliberately uses
planner **estimates** (`pg_class.reltuples`) for parts counts because an exact
`count(*)` over tens of millions of rows is too expensive to run per poll.

### `parts` indexes (the footprint that matters)

- `idx_parts_message_id (message_id)` — lookups by article id.
- `idx_parts_binary (binary_id)` — join parts to their binary.
- `idx_parts_grouping (group_id, norm_subject, poster) WHERE binary_id IS NULL`
  — **partial** index covering exactly the assembler backlog. This is the key
  design choice: it stays small (only unassigned parts) and its `reltuples`
  estimate is reused as the "unassigned parts" gauge.
- `UNIQUE (group_id, article_number)` — the natural key.

Every one of these grows with `parts`. The partial grouping index is the one
that stays bounded (it shrinks as parts get assigned), which is why it is cheap
to estimate from.

## Release search indexing

Release search tokenises the query and requires each token to appear in
`search_name` via `LIKE '%token%'`. The leading wildcard makes a plain btree
index unusable, so a **GIN trigram index** (`pg_trgm`) backs the search:

```sql
CREATE EXTENSION IF NOT EXISTS pg_trgm;
CREATE INDEX idx_releases_search_name_trgm ON releases USING gin (search_name gin_trgm_ops);
```

Measured plan for a substring search (with `enable_seqscan=off` to reveal
eligibility) is a `Bitmap Index Scan on idx_releases_search_name_trgm`, i.e. the
index serves the `LIKE '%token%'` predicate rather than a sequential scan. The
query semantics are unchanged; the index only makes broad searches scale as the
catalog grows. `pg_trgm` is a trusted extension (PostgreSQL 13+), so the DB
owner can create it without superuser; restricted deployments should create the
extension once as a superuser.

## Where the load concentrates

1. **Ingest writes** — bulk `INSERT` of parts during scans (batched, `batch_size`
   default 10000). This is the steady write load.
2. **Assembler churn** — `UPDATE parts SET binary_id = …` moves rows out of the
   partial index, then the stale-pruner `DELETE`s parts of incomplete binaries
   older than 14 days (`assembler` `StaleAfter`). Updates + deletes generate dead
   tuples → autovacuum pressure on `parts`.
3. **Search** — tokenised `LIKE` over `releases.search_name` (small table,
   indexed), cheap relative to parts.

## Retention: the main long-term decision

Today, parts that have been **assigned to a binary** are not pruned; only parts
of *stale incomplete* binaries are deleted (after 14 days). So assigned parts —
the bulk of the table over time — accumulate indefinitely.

Trade-off: the segments needed to **regenerate an NZB** live in the parts /
`release_files` data. Post-processing copies the ordered segments a release
needs into `release_files`, so in principle assigned parts for **released**
binaries are recoverable/derivable and are candidates for pruning. Options,
least to most invasive:

1. **Do nothing** — simplest; storage grows with everything indexed. Fine for
   modest group sets.
2. **Retention window on assigned parts** — delete parts for binaries released
   more than N days ago, provided `release_files` holds the segments needed to
   rebuild the NZB. Biggest storage win; requires verifying NZB generation reads
   only from `release_files` (not `parts`) for released items before enabling.
3. **Time partitioning of `parts`** (by ingest date, `created_at`) — makes
   dropping old data an `O(1)` partition drop instead of a big `DELETE`, and
   keeps autovacuum per-partition. Worth it once `parts` is very large or a
   retention policy is adopted; adds schema/operational complexity. This is now
   implemented as an opt-in rollout (#119) — see `docs/parts-partitioning.md`
   for the partition key, application-managed partition creation/expiry, and the
   resumable conversion procedure for existing installs.

Recommendation: keep option 1 until storage is a real constraint; when it is,
adopt option 2 (retention on released binaries' parts) after confirming NZB
regen is satisfied by durable segments, and consider option 3 (partitioning)
only at very large scale.

**Option 2 is now implemented (#118).** Raw-part retention is opt-in
(`GOINDEX_RETENTION_ENABLED`, `GOINDEX_RETENTION_DAYS`, plus `_INTERVAL`,
`_BATCH_SIZE`, `_MAX_BATCHES_PER_RUN`) and conservative by default (disabled).
A part is prunable only when its binary is released, the release is fully
post-processed (`pp_status='done'`), the release has durable segments
(`releases.segments <> '[]'`, from #105) so its NZB is reconstructable, and it
is older than the window. Everything else is retained: unassigned backlog,
incomplete/unreleased binaries, releases pending/failed post-processing, and
non-reconstructable releases. A dry-run preview
(`GET /api/v1/admin/retention/preview`) reports candidate counts, estimated
bytes, oldest/newest, and why rows are retained; the prune
(`POST /api/v1/admin/retention/prune`, or the background loop) deletes in
bounded, resumable, cancellable batches so it never holds one large
transaction.

## Autovacuum and statistics

Because `parts` is high-churn (bulk insert + assign updates + stale deletes) and
because the app relies on planner **estimates** for its parts gauges, keeping
autovacuum/analyze current matters both for bloat and for gauge accuracy.

Suggested per-table settings for `parts` (apply with `ALTER TABLE`):

```sql
ALTER TABLE parts SET (
  autovacuum_vacuum_scale_factor = 0.02,   -- vacuum after ~2% churn (default 20%)
  autovacuum_analyze_scale_factor = 0.02,  -- analyze more often -> fresher estimates
  autovacuum_vacuum_cost_limit = 2000      -- let vacuum keep up on a busy table
);
```

Rationale: the default 20% scale factor means autovacuum waits until millions of
dead tuples accumulate on a large table; lowering it keeps bloat and estimate
drift down. These are safe, reversible, table-local settings.

## Connection pool sizing and resource budgeting (#117)

goindex draws on **two independent pools** with very different sizes, and the
pipeline plus the HTTP API/admin control plane all share them:

- **NNTP pool** — bounded by the provider's connection limit
  (`GOINDEX_NNTP_MAX_CONNS`, or the active news server's `max_conns` column when
  one is configured; this is the *effective* NNTP capacity, per #104).
- **PostgreSQL pool** — `GOINDEX_DB_MAX_CONNS` (default **10**), shared by the
  scan/assemble/build/post-process/enrich loops **and** every Newznab search and
  admin request.

The NNTP-bound stages (scan, post-process) also do database work, so sizing
their concurrency purely off the NNTP budget can let pipeline workers consume
the whole (much smaller) DB pool, leaving searches and admin requests blocked on
connection acquisition — the service looks hung even though it is only queuing.

To prevent that, startup computes a **resource budget** that respects both
pools at once:

1. Auto-derive per-stage worker counts from the effective NNTP budget
   (scan ≈ ½ capped `[1,8]`; post-process ≈ ½ capped `[1,4]`).
2. Reserve DB headroom for the API/control plane — `GOINDEX_DB_RESERVED_CONNS`
   (default **auto**: ≈ ¼ of the pool, `[1,4]`; always leaves ≥ 1 for the
   pipeline). Set to `0` to disable (unsafe under load).
3. Clamp the combined pipeline worker footprint to
   `max_conns − reserved_conns` (the **DB pipeline budget**) so the reserved
   connections stay available for the API. Because the pools also hard-cap work
   (the pgx pool and the NNTP semaphore both make callers *wait* rather than
   fail), excess demand queues instead of deadlocking or starving the API.

Explicit overrides (`GOINDEX_SCAN_CONCURRENCY`) are honoured verbatim, but if the
resulting worker count overcommits the DB pipeline budget the server logs a
warning at startup — raise `GOINDEX_DB_MAX_CONNS` or lower the override.

The effective sizing is logged at startup (`nntp_max_conns`, `db_max_conns`,
`db_reserved_api_conns`, `db_pipeline_budget`, `scan_concurrency`,
`postprocess_concurrency`) and exposed to the admin health endpoint.

### Observing pool pressure

Prometheus exposes both pools so you can see contention and confirm the API has
headroom:

- NNTP: `goindex_nntp_pool_open_connections`, `..._idle_connections`,
  `..._max_connections`.
- PostgreSQL: `goindex_db_pool_total_connections`, `..._idle_connections`,
  `..._acquired_connections`, `..._max_connections`.
- **Saturation**: `goindex_db_pool_empty_acquires_total` (acquisitions that had
  to wait on an empty pool) and `goindex_db_pool_acquire_wait_seconds_total`
  (cumulative wait). A rising empty-acquire count or wait time means the pool is
  saturated — raise `GOINDEX_DB_MAX_CONNS` (keeping `instances × pool ≤
  PostgreSQL max_connections` with headroom) or lower pipeline concurrency.
- **Budget**: `goindex_db_reserved_api_connections` and
  `goindex_db_pipeline_budget_connections` show the static split.

PostgreSQL `max_connections` defaults to 100, so the default pool of 10 is
comfortable for a single instance. For many instances, use a pooler (PgBouncer);
it is unnecessary at single-instance scale.

## Server settings (containerised single node)

Starting points, to be scaled to the container's memory (these are guidance, not
one-size-fits-all):

- `shared_buffers` ≈ 25% of container RAM.
- `effective_cache_size` ≈ 50–75% of container RAM (planner hint; not an
  allocation).
- `work_mem` — modest (e.g. 16–64MB); it is per-sort/hash **per operation**, so
  large values × many concurrent ops can exhaust memory.
- `maintenance_work_mem` — higher (e.g. 256MB–1GB) to speed up vacuum and index
  builds on `parts`.
- Ensure `ANALYZE` runs often enough (see autovacuum tuning) so the estimate-based
  gauges and the planner stay accurate.

Apply via the Postgres image's config (command flags, a mounted `postgresql.conf`,
or `ALTER SYSTEM` + reload). None of these require application changes.

## When to revisit

- `parts` row count or on-disk size crosses your storage comfort zone → adopt a
  retention policy (and consider partitioning).
- Autovacuum can't keep up (rising dead-tuple ratio, bloat) → lower scale factors
  further / raise cost limit, or partition.
- Pool-wait or `too many clients` errors → raise pool size within
  `max_connections`, or add a pooler when running multiple instances.
- Planner estimates visibly diverge from reality (gauges look wrong) → analyze
  more aggressively.

## Summary

The system is already designed around the fact that `parts` is the giant
(partial index for the backlog, estimate-based gauges). The two highest-value
operational levers are **autovacuum/analyze tuning on `parts`** (cheap, safe,
immediate) and, when storage becomes a constraint, a **retention policy on
released binaries' parts** (with partitioning as the large-scale form). Server
and pool settings are standard and only need attention under observed pressure.
