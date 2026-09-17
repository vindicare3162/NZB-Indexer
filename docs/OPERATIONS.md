# Operations

Entry point for running goindex. Detailed procedures live in the documents
linked from each section. For diagnosis see `docs/RUNBOOK.md`.

## Deploy

The image is built outside the server and loaded as `goindex:latest`; there is
no source tree on the Unraid host. Compose lives at
`/mnt/user/appdata/goindex/docker-compose.yml`.

**Before deploying, confirm what you are deploying.** The running version string
is currently hand-written (`lb-20260916-2135`) and does not identify a commit.
At the time of writing three commits are unpushed and one of them —
`fd0723d` — was made five minutes *after* the running image was built and has
never executed in production.

```bash
git log origin/main..HEAD          # must be empty before you claim a fix is live
docker ps --filter name=goindex --format '{{.Image}} {{.Status}}'
docker inspect goindex --format '{{.Created}}'
```

Issue #195 replaces the hand-written string with the git SHA and exposes it on
`/api/v1/health`. Until it lands, the check above is the only reliable method.

Unraid specifics: `docs/deployment-unraid.md`.

## Migrations and index builds

Schema migrations run at application boot via `golang-migrate`. This is fine for
DDL that is fast, and dangerous for DDL that is not — `parts` is 1.8B rows and
1283 GB.

**Rules:**
- Only one `CREATE INDEX CONCURRENTLY` per table at a time. Two on `parts`
  deadlock; one incident lost ~2.5 hours when Postgres killed a build at 91% of
  validation.
- `CREATE INDEX CONCURRENTLY` cannot run inside a transaction.
- Build large indexes as an explicit operator step, not at boot. Tracked in #203.

## Backfill

Backfill is enabled globally by `scan.backfill_days` or
`scan.backfill_max_articles`, or per group by a backfill target. It walks
backwards from `groups.backfill_low` toward the server low.

Forward scans take strict priority and preempt backfill between groups, so a
large historical backfill cannot starve indexing of new posts.

**Judging progress:** watch `groups.backfill_low` decrease. Do not use
`last_scan_at` or `last_backfill_at` — see `docs/RUNBOOK.md` §2.

Bringing a new group online: add it paused, then enable one at a time.
`alt.binaries.boneless` is ~85.6 billion articles behind and must be brought up
alone.

## Maintenance tasks

Scheduled in `internal/server/maintenance.go`: `yenc-repair` (1m),
`yenc-verify` (2m), `retention`, `retry-failed`, `analyze`, `job-cleanup`,
`backup-verify`. Sizing rationale is in the source comments — the two yEnc
passes deliberately get unequal shares of the NNTP budget.

The scheduler does **not** skip an overlapping run. Tasks are sized to finish
inside their interval for that reason; overlapping passes multiply NNTP
connection demand, which has previously exhausted the provider's limit.

Detail: `docs/maintenance.md`.

## Re-assembly of mis-grouped parts (#196)

Subjects are parsed once, at ingest, so parts stored before the parser fix keep
their original grouping forever. Roughly 31.6% of parts gain a collection key
when re-parsed, and nearly all of them are already assembled — so the binaries
and the releases built from them have to be rebuilt too.

**Off by default.** Enable deliberately:

```
GOINDEX_MAINTENANCE_REASSEMBLE_ENABLED=true
GOINDEX_MAINTENANCE_REASSEMBLE_INTERVAL=1m
```

### What it does
Per batch, in one transaction: re-parse each part's stored subject, rewrite the
grouping fields that changed, detach those parts from their binary, delete the
releases built from it, then delete the binary. The normal assembler picks the
detached parts up on its next pass and groups them correctly. Assembly is not
reimplemented — only its inputs are repaired.

### What it will not touch
A binary backing a release with `pp_status = 'done'` is skipped entirely, parts
included. Those releases may already have been grabbed, and their GUIDs must not
change. The pass reports how many it protected.

### Before starting
**Pause the release builder**, or it converts mis-grouped binaries into releases
faster than re-assembly removes them. The builder is not paused by
`build_interval` alone — backlog-aware scheduling overrides it with
`AdaptiveMinInterval` whenever the loop is busy. Pause it by raising both, and
pin the other loops so only the builder stops:

```sql
INSERT INTO settings (key,value) VALUES
  ('schedule.build_interval','8760h'),
  ('schedule.postprocess_interval','30s'),
  ('schedule.downstream_interval','30s')
ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value, updated_at=now();
```
plus `GOINDEX_SCAN_ADAPTIVE_MIN_INTERVAL=8760h` in `.env`, then recreate the
container. `nextInterval` returns the configured value when the adaptive minimum
is not smaller, so pinning post-processing and assembly to 30s keeps them at
their current busy cadence while the builder stops.

Verify with a 5-minute window: release count delta should be 0 and no
`release build pass complete` lines should appear, while `assembly pass complete`
and `post-processing pass complete` continue.

### Throughput and what actually bounds it

Measured on this deployment: **250,000 rows/min at 1M rows per pass** — about
127 hours (5.3 days) over 1.9B parts.

That is the database's ceiling, not a scheduling one. At the 100k-per-pass
default the limit was passes-per-minute (150,000 rows/min); at 1M per pass each
pass takes ~4 minutes doing real work, so raising
`GOINDEX_MAINTENANCE_REASSEMBLE_BATCHES_PER_RUN` further only lengthens each
pass. Do not expect more from tuning.

**Measure progress with `parts_scanned` from the pass log, never the cursor.**
The cursor is a keyset over `parts.id`, and the purge left large id gaps the scan
crosses for free, so cursor velocity overstates progress wherever ids are sparse
— an ETA taken from it was out by roughly 2.5x.

A long pass logs nothing until it completes, so "no pass in the last 5 minutes"
does **not** mean stalled. Distinguish the two by sampling
`reassemble.parts_cursor` twice a minute apart.

### Monitoring and stopping
Progress is the `reassemble.parts_cursor` setting. Stopping is safe at any batch
boundary — set `GOINDEX_MAINTENANCE_REASSEMBLE_ENABLED=false` and restart; the
cursor persists and the next run resumes from it. A failed batch does not
advance the cursor, so nothing is skipped silently.

**There is no undo for a merge.** Re-grouped releases get new GUIDs.

## Capacity

`parts` is unpartitioned at 1283 GB with an "index everything, keep forever"
target, so it grows without bound. Partitioning and retention options are
tracked in **#182**; the design sketch is `docs/parts-partitioning.md`.

Do not run the re-assembly task (#196) and the partitioning work concurrently —
the same rows would be rewritten twice.

Disk pressure procedure: `docs/RUNBOOK.md` §3.

Storage is ZFS with ~3.4x compression. Sizes reported by Postgres are logical
and do not match disk usage; `df` free space on ZFS drifts with the compression
ratio and is not a dependable planning number. Use `zpool list` and
`zfs list -o name,used,avail,compressratio`. See HANDOVER §3.6.

## Credentials

Rotating the NNTP credential means updating the compose environment and
restarting. The application masks passwords in its own logs; surrounding
tooling has leaked one at least once. See #207.
