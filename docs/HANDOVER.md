# Handover

**Purpose.** This document exists so that someone with no prior exposure to
goindex can operate and extend it without asking the previous maintainer.
Everything here is either non-obvious from the code or was learned the hard way
in production. If you are new, read this before `docs/ARCHITECTURE.md`.

Last updated: 2026-09-17 (Gauntlet Iteration 1).

---

## 1. What this system is

A self-hosted Usenet NZB indexer. It pulls article headers from newsgroups over
NNTP, groups them into releases, and serves them to Sonarr/Radarr/Prowlarr via a
Newznab-compatible API plus downloadable `.nzb` files.

Go 1.27, PostgreSQL 17, Docker Compose on Unraid.

## 2. Current production state

Measured 2026-09-17 on `192.168.1.51`.

| Thing | Value |
|---|---|
| `parts` | 1.8B rows, 1283 GB (996 GB heap + 287 GB indexes), **unpartitioned** |
| `binaries` | 50.8M rows, 24 GB |
| `releases` | ~2.55M rows, 35 GB |
| Free space | ~865 GB on the btrfs cache pool |
| Active groups | 8 of 9 (`alt.binaries.boneless` deliberately paused) |
| Ingest rate | ~5.9M articles/hour |
| Users / API keys | **1 / 1** |

### Things that are true and surprising

- **`release_identifiers` is empty.** All ID-based search (tvdbid/imdbid/tmdbid)
  returns zero results. The read path and the write function both exist;
  nothing calls the writer. See issue #194. This is the single most important
  open defect.
- **36% of releases are category 8000 (Other)** and therefore invisible to
  Sonarr/Radarr category queries. See #197.
- **64% of releases are `pp_status='pending'`** (2.09M). Post-processing is the
  stage that resolves names and NFOs.
- **`alt.binaries.boneless` is paused on purpose.** It is ~85.6 billion articles
  behind and is the firehose that drove an earlier runaway. Bring it online
  alone, never alongside another new group.

## 3. Bus-factor knowledge

These are the things that were only ever in one person's head.

### 3.1 The recurring failure class: `ORDER BY … LIMIT` with an uncovered filter

A query of the form `SELECT … WHERE <filter> ORDER BY <col> LIMIT n`, where the
index providing the ordering does not cover the filter, performs well while
matching rows are dense and **silently degrades to a full scan** as they thin
out. It does not fail when written, reviewed, or first deployed. It fails weeks
later, under statement timeout (SQLSTATE 57014).

**This has caused four separate production incidents in this codebase.** If you
write a `LIMIT` query against `parts`, `binaries`, or `releases`, assume you are
about to cause the fifth. Tracking issue: #201.

### 3.2 `collected_parts` is inflated and must not be trusted

The assembler accumulates `distinct_files` per batch instead of recomputing per
binary, so a 93-file post reports 911. Any heuristic built on this column is
wrong. It once produced a confident finding of "13,055 merged collections" that
turned out to be zero merged collections. Issue #200.

### 3.3 Parsing happens once, at ingest

`ParseSubject` runs when a part row is created and the result is stored. A
parser fix therefore **only affects parts ingested after the fix is deployed**.
Historical data keeps its original grouping forever unless explicitly re-parsed
*and re-assembled* (#196). This is why a "fixed" parser can coexist with a flood
of badly grouped new releases — the parts are old, the assembly is new.

### 3.4 Timestamps are not liveness signals

- `last_scan_at` is **not** updated by backfill passes.
- `last_backfill_at` is NULL for every group in the deployed build.
- The scanner runs a roughly 30-minute duty cycle, so any monitoring window
  shorter than that can legitimately observe zero work.

Two separate "outage" investigations turned out to be these artefacts. **Measure
group liveness by backfill cursor movement (`groups.backfill_low` decreasing),
not by timestamps.** Issue #198.

### 3.5 Postgres and btrfs space behaviour

`DELETE` marks space reusable but does not shrink files. On this deployment the
underlying btrfs pool reclaimed physical space anyway, so free space recovered
without a `VACUUM FULL`. Do not generalise this to other filesystems.

### 3.6 Index operations on `parts` are dangerous

`CREATE INDEX CONCURRENTLY` on a 1.8B-row table takes hours. **Two `CONCURRENTLY`
operations on the same table deadlock** — during one incident Postgres killed a
build at 91% of validation, losing ~2.5 hours. Run them strictly one at a time.
Issue #203.

### 3.7 Shell hazards that have already bitten

- Usenet message-ids contain `$`. Unquoted, the shell mangles them and working
  articles appear to be 430 "no such article". Use `set -f` and quote.
- `pkill -f <script>` can match the ssh command string that launched it and kill
  your own session.
- A stuck Postgres `DO` block survives `pg_cancel_backend`; use
  `pg_terminate_backend`.

## 4. In-flight work

| Commit | State |
|---|---|
| `10e7259` | Committed, **unpushed**. Obfuscated-post indexing fixes. |
| `fd0723d` | Committed, **unpushed, never deployed** — made 5 minutes after the running image was built. Counter-less collection grouping, raised yEnc throughput. |
| `4d6832b` | Committed, **unpushed**. Quoted-title parser fix, banner gap, duplicate-counter guard. |

The running image reports `lb-20260916-2135`, a hand-written string with no link
to a commit. Issue #195 replaces it with the git SHA. **Until that lands, you
cannot tell what is running from the outside.**

## 5. Known gaps

See the open issue list. The S1 items are #194 (ID search), #195 (deploy),
#196 (re-assembly), #197 (categorisation), #198 (observability), #199 (yEnc
capacity).

Two issues are **launch blockers** that must close before a second person is
given access: #204 (quotas, tenancy, audit) and #205 (takedown mechanism).

## 6. If I got hit by a bus

1. Read §3 of this document. It is the part that is not recoverable from code.
2. `docs/ARCHITECTURE.md` for the pipeline and data model.
3. `docs/RUNBOOK.md` for "why is release X missing?".
4. The open GitHub issues are the real backlog; they carry triage rationale.
5. Nothing is "done" because it was discussed. Check `git log origin/main..HEAD`
   before believing any fix is live.
