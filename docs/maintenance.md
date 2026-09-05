# Automated maintenance jobs (#130)

goindex runs routine housekeeping as scheduled, observable jobs. Each task has
independent enablement and cadence, is wrapped in a persistent pipeline job (so
it appears in the admin **Jobs** panel as a `maintenance:<task>` entry), and
publishes a notification (#137) on completion or failure. A failing task never
stops the others, and all tasks stop promptly on shutdown.

## Tasks

| Task | Config key | What it does | Default |
| --- | --- | --- | --- |
| Retention prune | (enabled via `retention`) | Deletes raw parts for reconstructable, released, fully-processed releases older than the retention window, in bounded batches (#118). Destructive; gated by `retention.enabled` + `retention.days`. | off |
| Retry failed | `maintenance.retry_failed` | Re-queues failed post-processing releases so transient provider issues recover automatically (#132). | off, 1h |
| Analyze | `maintenance.analyze` | Refreshes PostgreSQL planner statistics (`ANALYZE`) over the core tables. Non-destructive. | off, 24h |
| Job cleanup | `maintenance.job_cleanup` | Prunes terminal job history older than `maintenance.job_retention` so the jobs table stays bounded. | on, 6h (retain 7d) |
| Backup verify | `maintenance.backup_verify` | Read-only backup-readiness check: confirms the database size is readable and each key table is reachable. Does **not** run or restore a dump. | off, 24h |

## Configuration

```yaml
retention:
  enabled: true        # gates the retention prune task
  days: 30
  batch_size: 5000
  max_batches_per_run: 0   # 0 = drain fully each run

maintenance:
  retry_failed:  { enabled: true,  interval: 1h }
  analyze:       { enabled: true,  interval: 24h }
  job_cleanup:   { enabled: true,  interval: 6h }
  job_retention: 168h        # 7 days
  backup_verify: { enabled: true,  interval: 24h }
```

Every field also has an environment override:
`GOINDEX_MAINTENANCE_RETRY_FAILED_ENABLED`,
`GOINDEX_MAINTENANCE_RETRY_FAILED_INTERVAL`,
`GOINDEX_MAINTENANCE_ANALYZE_ENABLED` / `_INTERVAL`,
`GOINDEX_MAINTENANCE_JOB_CLEANUP_ENABLED` / `_INTERVAL`,
`GOINDEX_MAINTENANCE_JOB_RETENTION`,
`GOINDEX_MAINTENANCE_BACKUP_VERIFY_ENABLED` / `_INTERVAL`.

## Observability

- **Job history**: each run creates a `maintenance:<task>` job that transitions
  `queued → running → completed|failed`, visible in the admin Jobs panel and at
  `GET /api/v1/admin/jobs`. The completion summary (e.g. "pruned 5 raw parts")
  or error message is recorded on the job.
- **Notifications**: retention completion emits `retention.completed`, backup
  verification emits `backup.outcome`, and any task failure emits an event with
  the task name and job id — delivered to configured webhooks (#137) and shown
  in the admin Notifications panel.
- **Metrics**: pipeline/DB/NNTP health metrics (#129) reflect the effects of
  maintenance (e.g. `goindex_parts_unassigned` falling after a prune).

## Safety notes

- The retention prune is the only destructive task; it only removes raw parts
  that remain reconstructable from durable NZB segments (#105), and it supports
  a dry-run preview and a batch cap (#118).
- Backup verification is deliberately read-only. Taking and restoring real
  `pg_dump` backups is left to external tooling; this task surfaces obvious
  pre-conditions (unreachable tables, permission loss) as an observable job.
- `ANALYZE` takes no locks that block reads or writes.
