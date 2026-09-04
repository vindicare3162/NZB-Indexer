# Time-partitioning the `parts` table (#119)

_Status: operational rollout guide. Tracking issue: #119._

At large scale the `parts` table dominates storage and write throughput (see
`docs/postgres-tuning.md`). Retention (#118) prunes redundant rows, but deleting
hundreds of millions of rows with ordinary `DELETE`s creates long transactions,
dead tuples, vacuum pressure, and index bloat. **Native declarative RANGE
partitioning by ingest month** lets retention drop an entire expired month as an
instant metadata-only `DROP TABLE`, and keeps autovacuum per-partition.

Partitioning is **opt-in** and operator-driven: it changes the natural key and
requires migrating existing rows, so goindex does **not** convert `parts`
automatically. The application works on both layouts; the partition-management
functions are safe no-ops on an unpartitioned table.

## Partition key

Partition by **`created_at`** (ingest time), one partition per month
(`parts_YYYY_MM`, half-open range `[month, next month)`).

- Ingestion never sets `created_at` explicitly (it defaults to `now()`), so new
  rows route into the current-month partition automatically.
- Retention/expiry is naturally aligned to ingest age.

PostgreSQL requires the partition key in every `UNIQUE`/`PRIMARY KEY`
constraint, so on a partitioned `parts`:

- `PRIMARY KEY (id, created_at)` (was `PRIMARY KEY (id)`).
- `UNIQUE (group_id, article_number, created_at)` (was `UNIQUE (group_id,
  article_number)`).

The `id` sequence stays global (`BIGSERIAL`), so `id` remains effectively
unique for the retention prune's batching cursor.

## Application compatibility

- **Ingestion** (`InsertParts`) COPYs into a plain temp table then
  `INSERT ... ON CONFLICT (group_id, article_number) DO NOTHING`. On a
  partitioned table the conflict arbiter must include the partition column:
  the unique index is `(group_id, article_number, created_at)`. Because a batch
  is always ingested "now", all its rows share the current month, so the
  arbiter still de-duplicates correctly within and across scans.
- **Assembly**, **release/NZB segment resolution**, **retention**, and
  **counts** key off `(group_id, norm_subject/collection_key, poster)`,
  `binary_id`, or `(group_id, article_number)` — all unaffected by partitioning.
- **Stats**: `PipelineStatistics` reads `pg_class.reltuples` for `parts` and the
  partial index. After partitioning, ensure `ANALYZE` runs on the parent so the
  aggregated estimate is populated.

## Rollout for an existing (unpartitioned) install

The conversion is resumable and does not require a long global lock; do it in a
maintenance window with ingestion paused. Outline:

```sql
BEGIN;
ALTER TABLE parts RENAME TO parts_old;

CREATE TABLE parts (LIKE parts_old INCLUDING DEFAULTS) PARTITION BY RANGE (created_at);
ALTER TABLE parts ADD PRIMARY KEY (id, created_at);
ALTER TABLE parts ADD UNIQUE (group_id, article_number, created_at);
CREATE INDEX idx_parts_binary ON parts (binary_id);
CREATE INDEX idx_parts_grouping ON parts (group_id, norm_subject, poster) WHERE binary_id IS NULL;
CREATE INDEX idx_parts_collection ON parts (group_id, collection_key, poster) WHERE binary_id IS NULL AND collection_key <> '';
COMMIT;
```

Then create partitions covering the existing data's month range and copy in
**resumable batches** (per month, so a failure/restart only redoes one month):

```sql
-- For each month M present in parts_old:
CREATE TABLE IF NOT EXISTS parts_YYYY_MM PARTITION OF parts
  FOR VALUES FROM ('YYYY-MM-01') TO ('next-month-01');
INSERT INTO parts SELECT * FROM parts_old
  WHERE created_at >= 'YYYY-MM-01' AND created_at < 'next-month-01';
```

When all months are copied and verified (`count(*)` matches), `DROP TABLE
parts_old`. Downtime is bounded by the rename+index step; the bulk copy runs
online per month.

The application's `EnsurePartsPartitions` can create the current and future
partitions from then on; no further manual partition DDL is needed.

## Ongoing operations (application-managed)

The store exposes partition management (no-ops when `parts` is unpartitioned):

- `EnsurePartsPartitions(now, future)` — create the current + next `future`
  monthly partitions (idempotent). Run periodically (e.g. daily) so a partition
  always exists before ingestion needs it.
- `CheckPartsPartitionCoverage(now)` — returns an actionable error when no
  partition covers `now`, for health/monitoring so a missing future partition
  is surfaced **before** an insert fails with "no partition of relation".
- `ListPartsPartitions()` — the current partitions and their bounds.
- `DropExpiredPartsPartitions(cutoff)` — drop whole partitions whose range lies
  entirely before `cutoff`, reclaiming storage without a table-wide `DELETE`.

### Expiry vs. row-level retention

`DropExpiredPartsPartitions` is a **coarse, time-only** expiry. The row-level
retention prune (#118) additionally guarantees it never removes parts still
needed by incomplete/unreleased binaries or non-reconstructable releases. Only
drop a partition once every row in it is unquestionably expired (e.g. well past
the retention window); otherwise run the row-wise prune, which honours the
reconstructability invariants.

## Monitoring

- Alert when `CheckPartsPartitionCoverage` errors (missing current/next
  partition) — ingestion would fail otherwise.
- Track partition count and per-partition size to confirm expiry is reclaiming
  storage.
