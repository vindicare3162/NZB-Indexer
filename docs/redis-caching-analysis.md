# Redis caching: analysis and recommendation

_Status: analysis / decision record. Tracking issue: #72._

## Question

As goindex grows (more indexed groups, more releases, more Newznab clients
polling), do we need Redis for caching, or is in-process caching enough?

## TL;DR recommendation

**Do not add Redis yet.** At single-instance scale the wins come from cheap
in-process caching and a couple of query tweaks, none of which need an external
cache. Redis earns its keep only once goindex runs as **more than one
instance**, because at that point some state (rate-limit counters, any shared
cache) must live outside the process. Adopt Redis at the horizontal-scaling
boundary, not before.

Concretely:

1. Cache the near-static reads in-process now (categories / newznab caps).
2. Reduce the per-request auth cost (the biggest hot path) with a short-TTL
   in-process API-key cache plus a throttled `last_used_at` update.
3. Leave search uncached for now; revisit with a short-TTL cache only if
   profiling shows it is a bottleneck.
4. Introduce Redis when (and only when) a second instance is deployed, starting
   with the rate limiter, then shared caches.

## Hot paths, grounded in the code

| Path | Frequency | Current cost | Cacheable? |
|------|-----------|--------------|------------|
| `auth.AuthenticateAPIKey` (`internal/auth/service.go`) | Every authenticated Newznab request | `GetAPIKeyWithUser` **+** `TouchAPIKey` = 2 DB round-trips per request | Yes — key→user is near-static; `last_used_at` can be throttled |
| `store.ListCategories` (`internal/store/categories.go`) | Every newznab `t=caps` and REST `/categories` | 1 query returning a small, effectively-static table | Yes — ideal in-process cache |
| `store.SearchReleases` (`internal/store/releases.go`) | Every search / RSS poll | Tokenised `LIKE` + `count(*)` over `releases` | Partially — result set changes as releases arrive; only a short TTL is safe |
| `auth.RateLimiter` | Every authenticated request | In-process map, already fast | Only matters for multi-instance (must be shared) |
| Discovery (`internal/server/discovery.go`) | Admin only, infrequent | Already cached in-process with a TTL | Already handled |
| Pipeline stats (`internal/store/stats.go`) | Admin status page | Already uses planner estimates to avoid full scans | Already handled |

The two paths that matter for scale are **API-key auth** (highest frequency)
and **categories** (trivially cacheable). Search is the heaviest single query
but its cacheability is limited by freshness expectations.

## Why not Redis at single-instance scale

- **Every cache Redis could hold, the process can hold faster.** For one
  instance, an in-memory `map` + `sync.RWMutex` (or `singleflight` for
  stampede protection) is a network hop cheaper than Redis and has no
  serialization cost.
- **Operational cost is real.** Redis adds a service to run, monitor, secure,
  back up (or explicitly treat as ephemeral), and reason about on failure
  (fail-open vs fail-closed). That is a poor trade when a `map` suffices.
- **Freshness/invalidation is the hard part, and it is identical** whether the
  cache is local or in Redis. Redis does not make invalidation easier here.

## What in-process caching buys us now (low risk)

1. **Categories / caps cache.** Load once, refresh on a long TTL (or on a
   settings/version bump). Removes a query from every caps and `/categories`
   call. Tiny, safe, high hit-rate.
2. **API-key auth cache.** Cache `apiKey → principal` for a short TTL (e.g.
   30–60s). Under a client polling every few seconds this collapses two DB
   round-trips per request down to ~one per TTL window. Pair it with a
   **throttled `TouchAPIKey`** (only write `last_used_at` at most once per
   minute per key) so the write path stops being per-request. Must invalidate
   on key revoke / user deactivation — bounded by the short TTL anyway.
3. **(Optional) short-TTL search cache.** Only if profiling shows search is
   hot. A 5–15s TTL keyed on the normalized filter would absorb duplicate
   client polls without noticeably staling results. Defer until measured.

All three are internal, reversible, and need no schema or infra changes.

## When Redis becomes the right call

The trigger is **horizontal scaling — running two or more goindex instances**
behind a load balancer. At that point:

- **Rate limiting must be shared.** The current in-process limiter would let a
  client get `N × instances` of its budget. This is the first thing to move to
  Redis (atomic `INCR` + `EXPIRE`, or a token-bucket Lua script).
- **Shared cache / session state.** If sessions or caches must be consistent
  across instances, a shared store (Redis) is the natural home.
- **Cross-instance coordination** (e.g. a distributed lock so only one instance
  runs a given job) may also want Redis, though the DB can serve that too.

Secondary triggers even at single instance: if the working set of cached data
grows beyond what we want to hold in the app's heap, or if we want cache to
survive restarts. Neither is true today.

## Recommended next steps

1. Land the **categories/caps in-process cache** (small, isolated).
2. Land the **API-key auth cache + throttled `last_used_at`** (biggest
   per-request win); measure DB query rate before/after via the metrics work
   (see the Prometheus task).
3. **Do not** add Redis until a second instance is on the roadmap. When it is,
   introduce Redis first for the **rate limiter**, then shared caches, behind a
   config flag so single-instance deployments keep the zero-dependency path.

## Decision

In-process caching now; Redis deferred to the multi-instance boundary. This
keeps the single-binary + Postgres deployment story intact (no new required
service) while removing the actual per-request DB pressure that grows with
client count.
