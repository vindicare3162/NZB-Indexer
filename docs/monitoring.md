# Monitoring goindex with Prometheus and Grafana

goindex exposes Prometheus metrics at `GET /metrics` on the same port as the
web UI and API. The endpoint is unauthenticated (Prometheus convention) and
exposes only operational counters — no secrets or user data.

## Metrics exposed

Pipeline depth (gauges, evaluated cheaply on each scrape):

- `goindex_parts_total` — estimated total parts rows.
- `goindex_parts_unassigned` — estimated parts not yet folded into a binary
  (the assembler backlog).
- `goindex_binaries_total`, `goindex_binaries_complete`,
  `goindex_binaries_unreleased`.
- `goindex_releases_total`.
- `goindex_releases_by_pp_status{status}` — releases per post-process status.
- `goindex_releases_failed_exhausted` — releases permanently failed (retry
  budget exhausted).

Worker activity:

- `goindex_worker_running` — 1 while a post-process cycle is in progress.
- `goindex_worker_cycles_total`, `goindex_worker_articles_pulled_total`,
  `goindex_worker_parts_inserted_total`, `goindex_worker_binaries_touched_total`,
  `goindex_worker_releases_created_total`, `goindex_worker_releases_renamed_total`,
  `goindex_worker_nfos_found_total`.

HTTP:

- `goindex_http_requests_total{method,route,status}` — the `route` label uses a
  bounded set of route patterns (e.g. `/api/v1/releases/:guid`) so per-request
  identifiers do not explode the series count.
- `goindex_http_request_duration_seconds{method,route}` — latency histogram.

Also: `goindex_metrics_scrape_errors_total` (increments if a scrape can't read
pipeline stats), plus the standard Go runtime and process collectors.

## Prometheus scrape config

```yaml
scrape_configs:
  - job_name: goindex
    metrics_path: /metrics
    static_configs:
      - targets: ["goindex:8080"]   # adjust host:port
```

## Grafana dashboard

Import [`docs/grafana/goindex-dashboard.json`](./grafana/goindex-dashboard.json)
and select your Prometheus data source when prompted. It includes pipeline
depth, worker throughput (rates), and HTTP request/error/latency panels.

## Notes

- If you need to restrict access to `/metrics`, put goindex behind a reverse
  proxy and allow the endpoint only from your Prometheus host, or firewall the
  port. The application does not gate `/metrics` itself.
- The pipeline gauges reuse the same cheap planner-estimate query as the admin
  status page, so scraping is inexpensive even with tens of millions of parts.
