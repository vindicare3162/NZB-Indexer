# ADR-001: Populate release identifiers, and stop advertising what we cannot serve

Status: Proposed (issue #194)
Date: 2026-09-17

## Context

Sonarr and Radarr search an indexer by external ID — `tvdbid`, `imdbid`,
`tmdbid` — in preference to free text. goindex advertises support for these in
its `caps` response and implements the matching query. The `release_identifiers`
table, however, has never contained a row: `Store.AddReleaseIdentifier` exists
and is tested, but no pipeline stage calls it.

The failure is silent. An ID query returns HTTP 200 with an empty result set,
which is indistinguishable from "this indexer genuinely has nothing".

## Options

1. **Do nothing.** Free-text search works; some clients fall back to it. Rejected
   — it leaves the primary integration path broken and undetectable.
2. **Populate identifiers only from the metadata enricher.** Clean, but the
   enricher is disabled by default and depends on an external provider, so
   coverage would be zero on a default install.
3. **Extract from NFO during post-processing, enrich for the rest, and gate
   caps on data presence.** Chosen.

## Decision

Extract IMDb, TVDB and TMDB ids from NFO text during post-processing, where the
NFO is already fetched and parsed. Extraction is a pure function over the NFO
(`postprocess.ExtractIdentifiers`), tested without a database, matching how
subject parsing is kept separate from storage; the write happens inside the
existing `ApplyPostProcessing` transaction so ids land atomically with the NFO
they came from.

No migration is required: `idx_release_identifiers_lookup (source, identifier)`
already exists from migration 0014. Once the table holds millions of rows,
`(source, identifier, release_id)` would let the correlated `EXISTS` in the
search query be answered from the index alone; revisit then, not now.

Separately, gate the advertised `supportedParams` on whether any identifier
exists. This is checked per caps request via `EXISTS (SELECT 1 … LIMIT 1)`
rather than sampled at startup, so advertisement starts automatically as
post-processing populates the table — no restart, and never stale.

## Consequences

- Post-processing does more work per release, against a 2.09M backlog.
- `caps` becomes dynamic; clients that cache it may not see capabilities appear.
  Tracked as an S3 item in #208.
- A failure of the existence check degrades to *not* advertising id search,
  which is the safe direction: a client that cannot use ids still works.
- Coverage is retroactive only as far as post-processing reaches, so ID search
  improves gradually rather than switching on.
- Advertising honestly means clients will correctly treat us as text-only until
  the data exists, which is a better failure than silent emptiness.
