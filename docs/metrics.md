# Metrics and observability (#129)

goindex exposes Prometheus metrics at `GET /metrics` (unauthenticated by
convention; it exposes only operational counters, never secrets). This document
describes the metric families, the naming and label policy, and example alerts
and dashboard panels.

## Naming and label policy

- All application metrics use the `goindex_` prefix.
- Counters end in `_total`; durations end in `_seconds`; point-in-time values are
  gauges.
- **Cardinality is bounded by design.** Labels are only used where the label
  domain is small and stable:
  - `route` on HTTP metrics is a normalized pattern (`/api/v1/releases/:guid`),
    never a raw path, so GUIDs and queries do not create series.
  - `server` on NNTP provider metrics is the operator-configured server name;
    operators configure a handful of providers.
  - `status` on `goindex_releases_by_pp_status` is the small fixed set of
    post-processing states.
  - Group-level health is exported as **aggregate scalars** (counts and maxima),
    never as one series per group, so thousands of groups add zero cardinality.

## Metric families

### HTTP
- `goindex_http_requests_total{method,route,status}` — request counter.
- `goindex_http_request_duration_seconds{method,route}` — latency histogram.

### Pipeline depth (from cheap pipeline statistics)
- `goindex_parts_total`, `goindex_parts_unassigned` (assembler backlog),
  `goindex_binaries_total`, `goindex_binaries_complete`,
  `goindex_binaries_unreleased`, `goindex_releases_total`,
  `goindex_releases_by_pp_status{status}`, `goindex_releases_failed_exhausted`.

### Worker activity
- `goindex_worker_running` (gauge), and counters
  `goindex_worker_cycles_total`, `_articles_pulled_total`,
  `_parts_inserted_total`, `_binaries_touched_total`, `_releases_created_total`,
  `_releases_renamed_total`, `_nfos_found_total`.

### API-key auth cache (#107)
- `goindex_apikey_cache_hits_total`, `_misses_total`, `_evictions_total`,
  `goindex_apikey_cache_size`.

### Connection pools (#117)
- NNTP: `goindex_nntp_pool_open_connections`, `_idle_connections`,
  `_max_connections`.
- PostgreSQL: `goindex_db_pool_total_connections`, `_idle_connections`,
  `_acquired_connections`, `_max_connections`,
  `goindex_db_pool_empty_acquires_total`,
  `goindex_db_pool_acquire_wait_seconds_total`,
  `goindex_db_reserved_api_connections`,
  `goindex_db_pipeline_budget_connections`.

### Group freshness (#129, aggregate)
- `goindex_groups_active` — active groups.
- `goindex_groups_behind` — active groups with positive forward lag.
- `goindex_group_lag_max_articles`, `goindex_group_lag_total_articles`.
- `goindex_groups_failing` — active groups with >= 1 consecutive failure.
- `goindex_group_consecutive_failures_max`.
- `goindex_group_oldest_success_age_seconds` — staleness of the least-recently
  successful active group.
- `goindex_groups_never_scanned`.

### NNTP provider health (#129, per configured server)
- `goindex_nntp_provider_circuit_state{server}` — 0 closed, 1 half-open, 2 open.
- `goindex_nntp_provider_consecutive_failures{server}`.
- `goindex_nntp_provider_failures_total{server}`,
  `goindex_nntp_provider_success_total{server}`,
  `goindex_nntp_provider_circuit_opens_total{server}`.
- `goindex_nntp_provider_pool_open_connections{server}`,
  `_pool_idle_connections{server}`.

### Scrape health
- `goindex_metrics_scrape_errors_total` — increments when a snapshot source
  errors during a scrape (e.g. a DB timeout), so a partial scrape is visible.

## Example alert rules

```yaml
groups:
  - name: goindex
    rules:
      # A provider's circuit is open (failing over or fully down).
      - alert: GoindexProviderCircuitOpen
        expr: goindex_nntp_provider_circuit_state == 2
        for: 5m
        labels: { severity: warning }
        annotations:
          summary: "NNTP provider {{ $labels.server }} circuit is open"

      # No group has scanned successfully in a long time (indexing stalled).
      - alert: GoindexScanStale
        expr: goindex_group_oldest_success_age_seconds > 86400
        for: 15m
        labels: { severity: warning }

      # Assembler backlog growing unbounded.
      - alert: GoindexAssemblerBacklog
        expr: goindex_parts_unassigned > 1000000
        for: 30m
        labels: { severity: warning }

      # PostgreSQL pool saturation: acquisitions repeatedly waiting.
      - alert: GoindexDBPoolSaturated
        expr: rate(goindex_db_pool_empty_acquires_total[5m]) > 0
        for: 10m
        labels: { severity: warning }

      # Releases permanently failing post-processing.
      - alert: GoindexReleasesFailedExhausted
        expr: increase(goindex_releases_failed_exhausted[1h]) > 0
        labels: { severity: info }
```

## Example Grafana panels

- **Freshness**: `goindex_group_oldest_success_age_seconds` (stat) and
  `goindex_groups_behind / goindex_groups_active` (gauge).
- **Backlog**: `goindex_parts_unassigned` and
  `goindex_releases_by_pp_status{status="pending"}` (time series).
- **Errors**: `sum by (server) (rate(goindex_nntp_provider_failures_total[5m]))`
  and `goindex_nntp_provider_circuit_state` (state timeline).
- **Capacity**: `goindex_db_pool_acquired_connections` vs
  `goindex_db_pool_max_connections`, and
  `rate(goindex_db_pool_acquire_wait_seconds_total[5m])`.
- **Throughput**: `rate(goindex_worker_articles_pulled_total[5m])` and
  `rate(goindex_worker_releases_created_total[5m])`.

Metric naming, types, and the label policy above are enforced by the collector
tests in `internal/metrics`, which also assert the group-health series count
stays bounded regardless of group count.
