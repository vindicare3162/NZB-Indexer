# Integration client fixtures (#140)

This document describes how goindex validates its Newznab-compatible API against
the ecosystem clients that consume it — Prowlarr, Sonarr, Radarr, SABnzbd, and
NZBGet — and how to optionally validate against real client instances.

## Automated fixture suite

The automated fixtures live in `internal/integration`. Unlike the mock-based
contract tests in `internal/api/newznab` (which validate response shape against
an in-memory repo), these run the **real** Newznab handler and NZB generator
over a **real, disposable PostgreSQL database** populated with releases that
carry durable NZB segments. This exercises the complete flow the way a client
does: capability discovery → search → details → download.

They require a test database and are skipped when `GOINDEX_TEST_DSN` is unset:

```
GOINDEX_TEST_DSN="postgres://goindex:goindex@localhost:55432/goindex_test?sslmode=disable" \
  go test ./internal/integration -count=1 -p 1
```

### What the fixtures cover

- **Capability discovery** (`t=caps`): advertised limits, the `search`,
  `tv-search`, and `movie-search` types, and the seeded category tree
  (Movies `2000`, TV `5000`, and subcategories). This is the request every
  client makes when you add the indexer.
- **Prowlarr / Sonarr / Radarr search flow** (`t=search`):
  - A text search returns an RSS feed whose items carry the fields these clients
    consume — `title`, `guid`, `category`, and an `enclosure` download link with
    `type="application/x-nzb"`.
  - A `cat=` filter narrows results to a category.
  - Pagination echoes the requested `offset` in the `newznab:response` element.
  - `t=details` returns the requested item.
  - The enclosure link resolves through `t=get` to a real NZB.
- **SABnzbd / NZBGet download flow** (`t=get`): the NZB response uses an NZB
  content type, carries a filename in `Content-Disposition`, is a well-formed
  `<nzb>` document with `<segment>` entries, and a bad id returns a Newznab
  error document (HTTP 200 with an `<error>` body) rather than a corrupt NZB, so
  downloaders treat it as a failed grab.

### API-key handling

The Newznab API-key contract (401 on a bad key, 429 with `Retry-After` under
rate limiting, and `X-RateLimit-*` headers) is enforced by the auth middleware
and is covered exhaustively in `internal/auth`. In production the handler is
mounted behind that middleware at `/api`.

## Optional live-client validation

These steps are **not** part of CI; they require running the real applications
and pointing them at a goindex instance. Content was rephrased for compliance
with licensing restrictions; consult each app's own documentation for exact
current steps.

1. Start goindex and create an API key for a user (Admin → API keys).
2. **Prowlarr**: add a *Generic Newznab* indexer with URL `http://<host>:8080`,
   API path `/api`, and the API key. Run the built-in "Test" — it performs a
   caps request and a search.
3. **Sonarr / Radarr**: add the same indexer (or sync it from Prowlarr). Trigger
   an interactive search for a known title and confirm results appear with
   working download links.
4. **SABnzbd / NZBGet**: grab a result from Sonarr/Radarr and confirm the
   downloader fetches the NZB (correct filename and content type) and reports a
   normal (not corrupt) job.

If any step fails, the automated fixture whose name matches the flow (capability
discovery, search flow, or download retrieval) is the fastest way to reproduce
and pin down the contract expectation.
