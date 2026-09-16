# Architecture

Entry point for understanding how goindex is put together. For operational
procedure see `docs/OPERATIONS.md`; for diagnosis see `docs/RUNBOOK.md`; for the
non-obvious history see `docs/HANDOVER.md`.

## 1. Pipeline

```
NNTP (XOVER/OVER headers)
  │   internal/scanner   — forward scan + backfill, per group, watermarked
  ▼
parts                    — one row per article. 1.8B rows / 1283 GB.
  │   internal/assembler — group parts into binaries by collection key
  ▼
binaries                 — one row per posted file-set. 50.8M rows.
  │   internal/release   — build a release once a binary is complete
  ▼
releases                 — one row per indexed release. 3.26M rows.
  │   internal/postprocess — resolve names, fetch NFO, categorise
  ▼
Newznab API (internal/api/newznab) + generated .nzb (internal/nzb)
```

Each stage is idempotent and resumable. Scanning persists its watermark after
every batch, so a crash mid-pass resumes from the last committed batch rather
than restarting the group.

## 2. Data model

### `parts`
One row per Usenet article header. Unique on `(group_id, article_number)` —
this is what makes ingest idempotent, and it is the most-scanned index in the
system (1.96B scans, 94 GB).

Grouping fields are computed **once, at insert**, by `scanner.ParseSubject`:

| Column | Meaning |
|---|---|
| `norm_subject` | subject with the segment counter stripped; groups single-file posts |
| `collection_key` | groups a multi-file post; empty when not recognised |
| `file_number` / `collection_files` | position within a collection, from an `[n/m]` counter |
| `part_number` / `total_parts` | segment position, from the yEnc `(n/m)` counter |

Because these are computed at insert, **a parser fix does not repair existing
rows**. See HANDOVER §3.3.

### Collection key forms

| Form | When |
|---|---|
| `t:<title>/<count>` | a release title precedes the file counter |
| `<archiveBase>/<count>` | no title, but the filename has an archive/parity extension |
| `<fileName>/<count>` | obfuscated post — the quoted name is shared across files |
| `b:<archiveBase>` | no counter at all; completeness settled on quiet time instead |

### `binaries`
Unique on `(group_id, norm_subject, poster)`. `complete` is set when the
collected parts satisfy the declared totals, or — for counter-less posts — by
`SettleQuietCollections` after a quiet period.

### `releases`
Unique on `release_hash`; `CreateRelease` uses `ON CONFLICT (release_hash) DO
NOTHING`, which is what collapses the same content cross-posted to several
groups into one release.

`category_id` is assigned at creation from `release.Categorize(name)`. Because
that name is often a fragment or an obfuscated hash, 36% of rows land in 8000
(Other) and stay there — see #197.

## 3. Index rationale

`parts` carries 287 GB of indexes. The ones that earn it:

| Index | Size | Scans | Why |
|---|---|---|---|
| `parts_group_id_article_number_key` | 94 GB | 1.96B | idempotent ingest |
| `parts_pkey` | 75 GB | 470M | identity |
| `idx_parts_binary` | 28 GB | 1.16B | fetch a binary's parts |
| `idx_parts_collection` | 12 GB | 30.9M | collection assembly |
| `idx_parts_grouping_single_known` | 6.9 GB | 27.1M | single-file assembly |

`idx_parts_grouping` is **63 GB for 42,008 scans** and is superseded by the
6.9 GB partial index above. It is a drop candidate, scheduled into the
partitioning window rather than done ad hoc (#182).

On `releases`, `idx_releases_search_name` (1.4 GB) and `idx_releases_posted`
(236 MB) report **zero scans**. This is not evidence they are useless — it is
evidence that no client traffic exists yet. Do not drop them before the
multi-user launch; re-measure after real Sonarr load.

## 4. Query patterns to avoid

**Never** write `WHERE <filter> ORDER BY <col> LIMIT n` against `parts`,
`binaries`, or `releases` unless one index covers both the filter and the
ordering. This pattern has caused four production incidents. It is fast in
testing and in early production, and degrades silently as matching rows thin
out. See HANDOVER §3.1 and issue #201.

Preferred alternatives already used in this codebase:
- keyset pagination (`(a,b,c) > (k.a,k.b,k.c)`) including a recursive-CTE
  skip-scan where a `GROUP BY … LIMIT` could not terminate early;
- driving from the small side with `CROSS JOIN LATERAL … LIMIT n`;
- partial indexes with an **explicit literal predicate** — the planner cannot
  prove a correlated lateral parameter satisfies a partial index predicate.

## 5. Component boundaries

- `internal/scanner` — NNTP → parts. Owns subject parsing.
- `internal/assembler` — parts → binaries. No NNTP access.
- `internal/postprocess` — releases → resolved releases. Owns NNTP body fetches.
- `internal/yencverify` — classifies counter-less parts by reading only the
  yEnc header line, then abandoning the response. ~250× cheaper in bandwidth
  than a full body, but ~2.5 s per article because the connection is spent and
  must be reopened.
- `internal/store` — all SQL. Parser and storage are deliberately decoupled:
  `ParseSubject` is a pure function over a string and is tested without a DB.
