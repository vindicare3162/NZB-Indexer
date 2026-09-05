# Optional derived OpenSearch release search (#139)

goindex can optionally mirror release records into an OpenSearch (or
Elasticsearch) index to provide richer fuzzy, ranked, and faceted release
search that scales independently of the primary database. This feature is
**disabled by default** and entirely optional.

PostgreSQL always remains the system of record. The OpenSearch index is a
*derived*, rebuildable accelerator for search only. Article ingestion, assembly,
pipeline state, deduplication, and NZB reconstruction never depend on it.

## When to enable it

Treat this as a deferred architectural option. Enable it only after you have
measured the limits of the tuned PostgreSQL search (see `docs/postgres-tuning.md`)
and confirmed a meaningful benefit on your workload. For single-instance
deployments, PostgreSQL-only search is the recommended default.

## Behaviour and consistency model

- When disabled (default), all release search runs against PostgreSQL exactly as
  before, including keyset pagination.
- When enabled, the REST `GET /api/v1/releases` endpoint and the API route
  queries OpenSearch first. If OpenSearch returns an error or is unreachable,
  the request transparently falls back to PostgreSQL, so a search-index outage
  never breaks search.
- The derived index is **eventually consistent** with PostgreSQL. It is
  populated by rebuilding from PostgreSQL (see below), so newly created releases
  become searchable in OpenSearch after the next rebuild. API consumers that
  require read-your-write consistency should continue to rely on the PostgreSQL
  path (disable OpenSearch) or account for indexing lag.
- OpenSearch pagination is offset-based (`from`/`size`); the opaque keyset
  cursor returned by the PostgreSQL path is a PostgreSQL-only optimization.

## Configuration

```yaml
opensearch:
  enabled: false            # off by default
  url: http://localhost:9200
  index: goindex-releases
  timeout: 10s
```

Environment overrides:

| Variable | Meaning |
| --- | --- |
| `GOINDEX_OPENSEARCH_ENABLED` | Enable the derived backend (`true`/`false`). |
| `GOINDEX_OPENSEARCH_URL` | OpenSearch base endpoint (required when enabled). |
| `GOINDEX_OPENSEARCH_INDEX` | Index name for release documents. |
| `GOINDEX_OPENSEARCH_TIMEOUT` | Per-request HTTP timeout (e.g. `10s`). |

When `enabled` is true, both `url` and `index` are required (validated at
startup).

## Rebuilding the index (replay and recovery)

Because PostgreSQL is authoritative, the index is always rebuildable from it.
Trigger a full rebuild with:

```
POST /api/v1/admin/search/reindex        (admin only)
```

The rebuild pages through every release in recency order and upserts a
denormalized document per release, keyed by GUID. Upserts are **idempotent**, so
the rebuild:

- is safe to re-run at any time,
- recovers the index after an OpenSearch outage or data loss,
- can be cancelled (the request context is honoured between pages) and simply
  started again to complete.

If no derived index is configured, the endpoint returns `503`.

## Document shape

Each release document is denormalized and contains only the fields needed for
search and faceting: GUID, name, lowercased search name, category id, size,
posted timestamp, and a recency sort key. No credentials or article contents are
indexed.

## Deployment, backup, and security

- **Deployment:** run OpenSearch separately from goindex and point `url` at it.
  goindex talks to OpenSearch over its REST API using the standard HTTP client;
  no additional client library is bundled.
- **Backup:** the OpenSearch index needs no backup of its own. It is derived
  from PostgreSQL and can be rebuilt at any time via the reindex endpoint, so
  back up PostgreSQL as usual and rebuild the index on restore.
- **Security:** restrict network access to OpenSearch to the goindex host, and
  place it behind authentication/TLS appropriate to your environment. The
  reindex endpoint is admin-only.

## Operational notes

- Search failures against OpenSearch are logged and fall back to PostgreSQL, so
  they degrade gracefully rather than failing requests.
- Rebuild after significant catalogue changes, or on a schedule, to bound
  indexing lag.
- Keep the PostgreSQL search well tuned regardless; it remains the fallback and
  the source of truth.
