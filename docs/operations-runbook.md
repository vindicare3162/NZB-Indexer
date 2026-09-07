# Operations runbook

## Service ownership

**Service:** goindex (self-hosted NZB indexer)
**Deployment:** Unraid at 192.168.1.51:8093
**Tech stack:** Go, PostgreSQL 17, Docker, Svelte 5, Alpine Linux runtime

## Quick reference

| Action | Command |
|---|---|
| Container status | docker ps --filter name=goindex |
| Container health | docker inspect goindex --format '{{.State.Health.Status}}' |
| API health | curl http://192.168.1.51:8093/api/v1/health |
| Live logs | docker logs --tail 50 -f goindex |
| Restart | docker compose -f /mnt/cache/appdata/goindex/docker-compose.yml restart |
| DB backup | See deployment-unraid.md |
| DB restore | See deployment-unraid.md |

## Monitoring

### Health checks

The container HEALTHCHECK runs every 30s. The readiness endpoint at GET /api/v1/ready verifies database connectivity.

Admin UI at http://192.168.1.51:8093/#/admin shows system health, pipeline stats, current tasks, jobs, capacity, and diagnostics.

### Prometheus

GET http://192.168.1.51:8093/metrics exposes pipeline depth, worker activity, NNTP pool, DB pool, and Go runtime metrics.

## Failure modes

### Container unhealthy but running

Check: docker logs --tail 50 goindex
Common causes: database unreachable, migration errors

### Container restarting

Check: docker logs goindex
Common causes: JWT secret not set, wrong DB credentials
Fix: verify .env, correct setting, docker compose restart

### Login fails

Check with direct API call:
  curl -X POST http://127.0.0.1:8093/api/v1/login -H Content-Type: application/json -d '{"username":"admin","password":"..."}'
Reset via CLI: docker exec goindex /usr/local/bin/goindex user add -username admin -password "<new>" -admin

### Pipeline backlog

Trigger post-process manually from admin UI. Check NNTP health and job history.

## Retention

Disabled by default. Enable via GOINDEX_RETENTION_ENABLED=true in .env. Preview before pruning via admin UI.

## Related docs

- deployment-unraid.md
- postgres-tuning.md
- monitoring.md
- README.md#backup-and-recovery
