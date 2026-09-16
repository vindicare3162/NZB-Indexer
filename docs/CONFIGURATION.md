# Configuration

Every setting is available in `config.yaml` and as an environment variable.
Environment wins. Defaults are in `internal/config/config.go`.

Blast radius is stated because several of these interact with a shared,
finite NNTP connection budget.

## Server / database

| Key | Env | Default | Blast radius |
|---|---|---|---|
| `server.listen` | `GOINDEX_SERVER_LISTEN` | `:8080` | — |
| `database.max_conns` | `GOINDEX_DATABASE_MAX_CONNS` | 10 | Shared by scan, post-process and API. Over-committing starves the API. |

## NNTP

| Key | Env | Default | Blast radius |
|---|---|---|---|
| `nntp.max_conns` | `GOINDEX_NNTP_MAX_CONNS` | 8 | **The scarcest resource.** Scanning, post-processing, and both yEnc passes draw from this pool. Exhausting it has previously failed whole batches at once. |
| `nntp.load_balance` | `GOINDEX_NNTP_LOAD_BALANCE` | false | Spreads across configured servers. This binding was missing entirely until 2026-09-16 and failed silently. |

## Scanning

| Key | Env | Default | Blast radius |
|---|---|---|---|
| `scan.interval` | `GOINDEX_SCAN_INTERVAL` | 30m | Sets the duty cycle. Monitoring windows shorter than this will see idle gaps — see RUNBOOK §2. |
| `scan.batch_size` | `GOINDEX_SCAN_BATCH_SIZE` | 10000 | Watermark is persisted per batch; smaller batches mean finer crash resume. |
| `scan.backfill_days` / `scan.backfill_max_articles` | `GOINDEX_SCAN_BACKFILL_*` | 0 / 0 | Either being non-zero enables backfill globally. Per-group targets also enable it. |
| `scan.postprocess_interval` | `GOINDEX_SCAN_POSTPROCESS_INTERVAL` | 5m | Post-processing is 2.09M releases behind; this is the drain rate lever. |

## Metadata / retention

| Key | Env | Default | Blast radius |
|---|---|---|---|
| `metadata.enabled` | `GOINDEX_METADATA_ENABLED` | **false** | While false there is no season/episode metadata and no external ID enrichment. Contributes to #194. |
| `retention.enabled` | `GOINDEX_RETENTION_ENABLED` | **false** | Opt-in. With the "index everything, keep forever" target this stays false, which is why `parts` grows without bound — see #182. |

## Auth

| Key | Env | Default | Blast radius |
|---|---|---|---|
| `auth.default_rate_limit` | `GOINDEX_AUTH_DEFAULT_RATE_LIMIT` | 100 | Per key, per window. **A single Sonarr season search with Prowlarr fan-out exceeds this.** See #204. |
| `auth.rate_limit_window` | `GOINDEX_AUTH_RATE_LIMIT_WINDOW` | 1h | Enforced in memory — a restart resets every quota. See #179. |
| `auth.jwt_secret` | `GOINDEX_AUTH_JWT_SECRET` | — | Rotating invalidates all sessions. |

## Secrets

The application masks `password=***` in its own startup logging. Operational
tooling around it has leaked a credential at least once by matching `PASS=`
where the variable was `PASSWORD=`. See #207.
