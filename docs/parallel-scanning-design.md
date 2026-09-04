# Design: bounded parallel group scanning

_Status: design / scoping. Tracking issue: #100. No implementation yet._

## Problem

`Worker.runScan` scans groups **sequentially** — `for _, g := range groups { ScanForward(g) }`
(`internal/worker/worker.go`). One forward pass over N groups therefore takes
roughly N × the time for a single group. At 50 groups this is usually fine; at
500 a full pass can exceed `ScanInterval`, so the loop runs back-to-back and the
newest groups' forward scans are delayed. The "Current tasks" progress counter
(#98) makes this visible but does not improve throughput.

Goal: scan several groups **concurrently**, bounded by the NNTP connection
budget, without races on watermarks or metrics, and without starving forward
scans behind long backfills.

## Current behaviour (grounded in the code)

- **Scan loop** (`worker.go runScan`): resolves active groups via
  `ListGroups`, iterates sequentially, sets single-group `ScanProgress`, calls
  `scanner.ScanForward` / `ScanBackfill`, and aggregates
  `ArticlesPulled`/`PartsInserted` into `w.metrics` under `w.mu`. The whole pass
  is serialised by `scanMu` (so two passes never overlap).
- **Scanner** (`internal/scanner/scanner.go`): holds **no per-scan mutable
  state** (`Scanner{src, repo, log, opts}` are read-only during a scan). Each
  `ScanForward`/`ScanBackfill` uses only locals and its own `store.Group`
  snapshot, allocating its own `parts` slice per batch. **Safe to call
  concurrently for different groups.** One group scan = one `SelectGroupInfo`
  checkout + one `Overview` checkout **per XOVER batch** (connection released
  between batches; it never holds a connection for the whole group).
- **NNTP pool** (`internal/nntp/pool.go`): fixed-size, concurrency-safe. A
  `sem` of capacity `MaxConns` is the hard ceiling; `acquire` **blocks** when
  exhausted (until a slot frees or ctx cancels). Every group-scoped op
  re-issues `selectGroup` on checkout, so **sharing connections across groups is
  already safe** — there is no sticky-group optimisation to break. `MaxConns` is
  fixed at construction (`Reconfigure` preserves it); the ceiling can't grow at
  runtime.
- **Watermarks** (`internal/store/groups.go`): `UpdateGroupForwardPosition` and
  `UpdateGroupBackfillPosition` write **by group id**. Different groups don't
  contend; the **same** group scanned twice concurrently **would** race on its
  row. Persisted after every batch for resumability. No shared counters at the
  store layer — the pgx pool is concurrency-safe.
- **Existing parallelism** (`internal/postprocess/postprocess.go`
  `Processor.Run`): a bounded worker pool — jobs channel, N workers,
  `WaitGroup`, mutex-guarded counters, `stop`/`stopOnce` abort on fatal error,
  dispatcher `select`ing on `ctx.Done()`/`stop`/`jobs<-`. This is the pattern to
  mirror.
- **Shared budget**: the **same** `nntp.Pool` is injected into both the scanner
  and post-processor (`server.go`). So real concurrency across scan + pp +
  assemble/build is already capped by `MaxConns`; adding scan workers just
  increases demand on that one semaphore.

## Design

### 1. Bounded worker pool for group scans

Replace the sequential loop in `runScan` with a bounded pool modelled on
`postprocess.Processor.Run`:

- A `jobs` channel of `store.Group`; `scanConcurrency` worker goroutines each
  loop `for g := range jobs` calling `ScanForward`/`ScanBackfill` (per the pass
  type), checking `ctx.Err()` early.
- Result aggregation: each worker adds its `ArticlesPulled`/`PartsInserted`
  under `w.mu` (or aggregates locally and adds once at the end). The current
  bare `+=` is only safe single-threaded, so this must move under the lock.
- Errors: per-group scan errors are logged via `recordError` and the worker
  continues to the next group (matching today's `continue`). There is no
  "fatal" DB-apply step in scanning like pp has, so a simpler error model
  suffices — no global abort needed; a cancelled ctx stops all workers.
- `scanMu` still guards the whole **pass** (no two forward passes overlap), and
  because a single pass's `jobs` are distinct groups, **no group is scanned by
  two workers at once** — this preserves per-group watermark exclusivity for
  free. (Manual single-group triggers also go through `doScan`/`scanMu`, so they
  can't overlap a full pass on the same group.)

### 2. Concurrency sizing and the shared connection budget

The scanner and post-processor share one pool. The design must **not** let
scan + pp workers oversubscribe `MaxConns` so badly that everything blocks in
`acquire`. Proposal:

- Add `Scan.Concurrency` (env `GOINDEX_SCAN_CONCURRENCY`), default derived from
  `MaxConns` via a `scanConcurrency(maxConns)` helper (mirroring
  `ppConcurrency`). A sensible default: **half the pool, clamped to [1, 8]**,
  leaving headroom for pp (which already takes ~half, clamped [1,4]) and the
  occasional assemble/build (DB-only, no NNTP) — noting scan and pp loops run on
  independent tickers and won't always overlap.
- Because `acquire` blocks (never errors) on exhaustion, oversubscription
  degrades to serialisation rather than failure — safe but slow. Keeping the
  sum of scan + pp default concurrency ≤ `MaxConns` avoids that.
- Respect the fixed pool ceiling: even if `Scan.Concurrency` is set high, real
  NNTP parallelism is still capped by `MaxConns`. Document this so operators
  raise `GOINDEX_NNTP_MAX_CONNS` (and their provider's connection limit)
  together with scan concurrency.

### 3. Progress / metrics for multiple in-flight groups

`ScanProgress` currently models one group. Change to a **set**:

- Replace the single `*ScanProgress` with a collection — e.g.
  `ScanProgress []GroupScan` where `GroupScan{Group, Backfill}`, plus a
  `Completed`/`Total` counter for overall pass progress. `setScanProgress`
  becomes `scanBegin(group)` / `scanEnd(group)` (add/remove under `w.mu`), and a
  pass-level `total` + atomic-ish `completed` count.
- "Current tasks" (SPA) lists the in-flight groups (bounded by concurrency, so
  the list stays small) and shows overall `completed/total`.
- Prometheus: keep the existing counters; optionally add a
  `goindex_scan_in_flight` gauge. The single-group Prometheus adapter needs
  updating to the new shape.

### 4. Fairness / starvation

- Forward vs backfill already run as **separate passes** on their own cadence,
  so a long backfill does not block forward scans (different loop). Within a
  pass, the worker pool drains the group list; no single group can monopolise a
  pass because `ForwardMaxArticles`/`BackfillMaxArticles` already cap per-pass
  work per group and persist a resumable watermark.
- Ordering: dispatch groups in a stable order (e.g. by id) so progress is
  predictable; optionally prioritise groups with the largest gap
  (`server high − last_scanned_high`) in a later iteration — out of scope for v1.

## Risks and tradeoffs

- **Connection contention / provider limits**: more scan workers = more
  simultaneous NNTP connections. Providers cap concurrent connections; exceeding
  it causes auth/`too many connections` errors. Mitigate by defaulting
  conservatively and documenting the `MaxConns`↔`Scan.Concurrency` relationship.
- **DB write load**: parallel scans insert parts concurrently (bulk
  `InsertParts`) and update N different group rows. The pgx pool
  (`GOINDEX_DB_MAX_CONNS`, default 10) must have enough connections for scan
  workers + pp workers + API. Tie scan concurrency sizing to DB pool headroom
  too (see `docs/postgres-tuning.md`).
- **Same-group double scan**: must stay impossible. The design keeps `scanMu`
  around whole passes and only parallelises across distinct groups, so this is
  preserved — but any future "scan this one group now while a pass runs" feature
  must guard per-group (e.g. a per-group in-flight set) rather than relying on
  the pass-level mutex.
- **Error visibility**: `recordError` overwrites a single `LastError`;
  concurrent failures clobber it. Acceptable, but consider a small ring/count of
  recent scan errors if this becomes hard to debug.
- **Metrics shape change**: `ScanProgress` going from object to array is a
  breaking change to the `/api/v1/admin/status` payload and the Prometheus
  adapter; update the SPA and any consumers together.

## Recommended phased plan

1. **Config + sizing**: add `Scan.Concurrency` + `GOINDEX_SCAN_CONCURRENCY` and
   `scanConcurrency(maxConns)`; wire into a new worker/scanner option. Default
   preserves today's behaviour if set to 1.
2. **Parallel loop**: refactor `runScan` into the bounded worker pool
   (jobs channel + N workers + mutex aggregation + ctx cancellation), keeping
   `scanMu` at the pass level. Add a worker test asserting N groups scan
   concurrently and watermarks update per group.
3. **Progress model**: convert `ScanProgress` to a multi-group set; update
   "Current tasks" and the Prometheus adapter.
4. **Docs**: update README/monitoring with the concurrency knob and the
   `MaxConns` relationship; note provider connection limits.

Each phase is independently shippable behind the config default (concurrency 1 =
current behaviour), so the change can land incrementally and be validated live.

## Decision

Adopt the bounded-worker-pool approach mirroring post-processing, gated by a new
`Scan.Concurrency` knob sized to share the NNTP `MaxConns` budget with
post-processing. It reuses a proven pattern, needs no change to the
already-concurrency-safe pool or per-group watermark writes, and degrades safely
(blocking, not failing) under connection pressure. The main real work is the
worker-pool refactor and converting the single-group progress model to a set.
