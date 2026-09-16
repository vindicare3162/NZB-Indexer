# API

Two surfaces:

- **Newznab/Torznab** — what Sonarr, Radarr, Prowlarr, SABnzbd and NZBGet use.
  Wire-level detail and client request traces: `docs/newznab-compat.md`.
- **`/api/v1/*`** — the web UI's own REST API (session or API-key auth), plus
  `/api/v1/admin/*` for operator actions. Spec: `GET /api/v1/openapi.yaml`.

## Newznab compatibility status

| Function | State |
|---|---|
| `t=caps` | Implemented |
| `t=search` | Implemented (free text + category) |
| `t=tvsearch` / `t=movie` | **Text and category only — see below** |
| `t=get` | Implemented; NZB generated from the database |
| `t=details` | Implemented, but returns **no file list** (`release_files` is empty) |

## Known contract gap: ID search returns nothing

`caps` advertises `imdbid,tvdbid,tmdbid` under `tv-search` and `movie-search`,
and the search implementation does match on them
(`internal/store/releases.go:485`). But **`release_identifiers` contains zero
rows**, because nothing in the pipeline calls `Store.AddReleaseIdentifier`.

The practical effect is that any ID-based query returns an empty, successful
response. Sonarr and Radarr search by ID by preference, so the primary
integration path yields nothing while appearing healthy.

Tracked in **#194**, which both wires the write path and makes `caps` stop
advertising ID search while the table is empty — failing honestly rather than
silently.

## Category assignment

`category_id` is set when the release is created, from the release name. Names
that are fragments or obfuscated hashes cannot be classified, so **36% of
releases are category 8000 (Other)** and are excluded from the category-filtered
queries clients actually issue. Tracked in **#197**, which makes the category
re-derivable after post-processing resolves a better name.

Consequence for clients: a release's category can change after it is first
indexed.

## Paging

Offset-based. At depth over ~2.55M releases this is O(n); keyset paging is in the
S3 backlog (#208).

## Rate limiting

Per API key, default 100 requests per hour, enforced in process. A single Sonarr
season search behind Prowlarr fan-out will exceed it, and a restart resets all
counters. See #204 and #179.
