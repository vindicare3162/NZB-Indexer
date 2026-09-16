# ADR-002: Re-parse and re-assemble historical parts

Status: Proposed (issue #196)
Date: 2026-09-17

## Context

`ParseSubject` runs once, when a part row is inserted, and its output is stored.
A parser fix therefore only affects parts ingested afterwards.

Measured against a 36,351-article sample of production subjects, 31.6% of parts
gain a collection key when re-parsed — roughly 580M of 1.8B rows. Critically, 0
of 9,577 unassembled parts in that sample gained anything: every part that would
regroup is **already assembled** into a per-file binary with a release attached.

So a re-parse pass alone changes nothing observable. The split releases already
exist. Roughly 2.1M releases are per-file fragments; one 43-file post exists as
43 releases averaging 10 MB.

## Options

1. **Forward-only.** Accept the history as permanently wrong. Cheapest, and
   leaves clients seeing fragments indefinitely.
2. **Purge and re-ingest.** Clean, but discards 1283 GB of headers that took
   weeks to pull and cannot be re-pulled faster than the provider allows.
3. **Re-parse plus targeted re-assembly.** Chosen.

## Decision

A resumable maintenance task, keyset-paged over `parts.id` with its cursor in
`settings`: re-parse each batch, skip unchanged rows, mark changed rows' binaries
dirty, then tear down and re-assemble those binaries. Delete the stale release
only when `pp_status <> 'done'`.

## Consequences

- Rewrites up to 580M rows: roughly 120 GB of new tuples plus index churn,
  against ~865 GB free. Must be throttled and watched.
- **Re-grouped releases change GUID.** Clients that already grabbed a fragment
  may re-grab or lose history. There is no way to merge without this.
- A merge cannot be undone. The task is stoppable at a batch boundary, but
  completed merges stand.
- Must not run concurrently with the `parts` partitioning work (#182), or the
  same data is rewritten twice.
- Fixing grouping will resolve many release names, which should also reduce the
  36% sitting in category 8000 (#197). Sequence #197 after this.
