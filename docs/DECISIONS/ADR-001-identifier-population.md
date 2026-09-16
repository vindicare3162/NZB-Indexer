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

Extract IMDb ids from NFO text during post-processing, where the NFO is already
fetched and parsed. Populate TVDB/TMDB ids from the metadata enricher when it is
enabled. Add `(source, identifier) INCLUDE (release_id)` to support the lookup.

Separately, compute the advertised `supportedParams` from a startup count of
`release_identifiers`, so an empty table does not advertise ID search.

## Consequences

- Post-processing does more work per release, against a 2.09M backlog.
- `caps` becomes dynamic; clients that cache it may not see capabilities appear.
  Tracked as an S3 item in #208.
- Coverage is retroactive only as far as post-processing reaches, so ID search
  improves gradually rather than switching on.
- Advertising honestly means clients will correctly treat us as text-only until
  the data exists, which is a better failure than silent emptiness.
