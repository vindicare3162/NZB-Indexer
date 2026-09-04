# Newznab compatibility (#136)

goindex implements the Newznab XML API so Prowlarr, Sonarr, Radarr, SABnzbd, and
NZBGet can search and download NZBs. This documents the supported surface and
the client request traces the contract tests exercise
(`internal/api/newznab/contract_test.go`, plus the API-key/rate-limit contract
in `internal/auth`).

## Authentication and rate limiting

- API key is passed as the `apikey` query parameter (Newznab convention),
  enforced by the `RequireAPIKey` middleware.
- Missing/invalid key → HTTP 401.
- Over the per-key budget → HTTP 429 with `Retry-After` and `X-RateLimit-Reset`.
- Successful authenticated responses carry `X-RateLimit-Limit`,
  `X-RateLimit-Remaining`, and `X-RateLimit-Reset`.

## Functions (`t=`)

| `t` | Purpose | Notes |
|-----|---------|-------|
| `caps` (or empty) | Capabilities document | server info, limits, searching, categories |
| `search` | Free-text search | `q`, `cat`, `limit`, `offset` |
| `tvsearch` | TV search | `q`, `cat`, `season`, `ep`, `imdbid`, `tvdbid`, `tmdbid`, `limit`, `offset` |
| `movie` | Movie search | `q`, `cat`, `imdbid`, `tmdbid`, `limit`, `offset` |
| `details` | Single-item feed | `id` (release GUID) |
| `get` | Download the NZB | `id` (release GUID) |

Unknown `t` → newznab error code **202** ("No such function").

### Caps

- `<server>` advertises title and version; `<limits max default>` bound page
  size. Every param listed in a search type's `supportedParams` is one the
  handler actually resolves (a contract test enforces this — unsupported params
  are never advertised).
- Categories nest: parent categories (e.g. 2000 Movies, 5000 TV) contain
  `<subcat>` children (2040 Movies/HD, 5040 TV/HD). Subcategories never appear as
  top-level categories.

### Search feed (RSS)

- `<rss version="2.0" xmlns:newznab="…">` with a `<newznab:response offset total>`
  echoing the requested offset and the (capped) total for client paging.
- Each `<item>` has `title`, `guid`, `link`, `pubDate` (RFC1123Z), `category`,
  an `<enclosure url length type>` (absolute `t=get` URL, byte length,
  `application/x-nzb`), and `<newznab:attr>` for `size`, `grabs`, and `category`.
- `link` equals the enclosure URL. Empty results return a valid feed with
  `total="0"` and no items.
- `cat` accepts a comma-separated list; a parent category id also matches its
  children.

### get

- Returns `Content-Type: application/x-nzb` and
  `Content-Disposition: attachment; filename="…"`. A successful download
  increments the release's grab counter.
- Unknown id / missing NZB → newznab error code **300** ("No such item").
- Missing `id` → newznab error code **200** ("Missing parameter").

## Client traces exercised by the contract suite

Prowlarr/Sonarr/Radarr follow the same broad flow, validated end-to-end:

1. `GET /api?t=caps&apikey=…` — parse limits, searching params, categories.
2. `GET /api?t=tvsearch&q=show&season=3&ep=10&cat=5000&apikey=…` — season/ep
   folded into an `s03e10` token.
3. `GET /api?t=movie&q=film&cat=2000&apikey=…`.
4. `GET /api?t=search&q=…&limit=…&offset=…&apikey=…` — pagination echoed.
5. `GET /api?t=details&id=<guid>&apikey=…`.
6. `GET /api?t=get&id=<guid>&apikey=…` — NZB download, grab counted.

Error codes follow the Newznab convention (HTTP 200 with an `<error code
description>` body): 200 missing parameter, 202 no such function, 300 no such
item, 900 internal error.
