# Runbook

Diagnosis procedures. For architecture see `docs/ARCHITECTURE.md`; for the
history behind these checks see `docs/HANDOVER.md`.

## 1. "Why is release X missing?"

Work the pipeline in order and stop at the first stage where the row is absent.
Until #198 lands there is no single endpoint for this, so it is manual SQL.

**Always bound these queries.** `parts` is 1.8B rows; an unindexed predicate on
it will run for minutes and may time out. Never use `ILIKE` on `parts.subject`.

1. **Was the article seen?** Look it up by the indexed path only:
   `SELECT … FROM parts WHERE group_id = $1 AND article_number = $2`.
   If absent: the scanner has not reached that article number. Check
   `groups.backfill_low` and `groups.last_scanned_high`.
2. **Was it grouped?** Check `collection_key` and `norm_subject` on the part.
   Empty `collection_key` on a multi-file post means the parser did not
   recognise the subject style *at the time it was ingested* — see HANDOVER
   §3.3. Historic rows are not repaired by a parser fix.
3. **Did a binary form?** Join via `parts.binary_id`. NULL means the part is
   still unassembled — most often because it is awaiting yEnc verification
   (#199), which is a safe but slow state.
4. **Is the binary complete?** `binaries.complete`. If false, compare
   `collection_files` against the distinct filenames actually present.
   Do **not** trust `collected_parts` (HANDOVER §3.2).
5. **Did a release build?** Join `releases.binary_id`.
6. **Did post-processing run?** `releases.pp_status`. 64% of rows are `pending`;
   this is the most likely reason a release exists but looks wrong.
7. **Is it in a category the client queries?** 36% of releases are 8000 (Other)
   and are invisible to Sonarr's `cat=5030,5040` (#197).
8. **Is it findable by ID?** Currently never — `release_identifiers` is empty
   (#194). If the client searched by tvdbid/imdbid, this is the answer.

## 2. "A group has stopped scanning"

**Check cursor movement before believing this.**

```sql
SELECT name, backfill_low, last_scanned_high, server_high FROM groups WHERE active;
```

Sample twice, a few minutes apart. A decreasing `backfill_low` means the group
is working, regardless of what any timestamp says.

Known false positives, both of which have triggered unnecessary investigations:

- `last_scan_at` is not updated by backfill passes, and `last_backfill_at` is
  NULL in the deployed build. Timestamp-based starvation checks are unreliable.
- The scanner runs a ~30-minute duty cycle. A monitoring window shorter than
  that can legitimately see zero articles.
- A forward pass over 1,000,000 articles takes roughly an hour. A group can be
  mid-pass and silent for that whole time; it logs only on completion.

## 3. Disk pressure

The cache pool holds a 1283 GB `parts` table. If free space falls below ~200 G:

**First, get a real number.** The pool is ZFS and `df` free space drifts with the
compression ratio — a 119G swing was observed with nothing deleted. Use:

```bash
zpool list
zfs list -o name,used,avail,refer,compressratio
```

1. Check for a never-used index before deleting any data:
   ```sql
   SELECT relname, indexrelname, idx_scan, pg_size_pretty(pg_relation_size(indexrelid))
   FROM pg_stat_user_indexes ORDER BY pg_relation_size(indexrelid) DESC LIMIT 20;
   ```
   A previous incident was resolved by dropping a 183 GB index with zero scans
   since database creation.
2. `DELETE` will not shrink Postgres files. Whether the filesystem returns the
   space is a ZFS question, not a Postgres one — check `zfs list`, not `df`.
3. Do not start an index build and a bulk delete at the same time.

## 4. Index builds

Strictly one `CONCURRENTLY` operation per table at a time. Two on `parts` will
deadlock, and Postgres may kill a build near the end of validation after hours
of work. See HANDOVER §3.6.

## 5. Confirming what is actually deployed

Until #195 lands, the version string is hand-written and does not identify a
commit. Check `git log origin/main..HEAD` for unpushed work before concluding
that a fix is live. At the time of writing, three commits are unpushed and one
of them has never run in production.
