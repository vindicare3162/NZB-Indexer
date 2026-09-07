# Unraid deployment

This document describes the production goindex deployment on Unraid at `192.168.1.51`.

## Deployment layout

| Component | Details |
|---|---|
| Application container | `goindex` |
| Database container | `NZB-postgresql17` (shared PostgreSQL 17) |
| App data | `/mnt/cache/appdata/goindex/` |
| Compose file | `/mnt/cache/appdata/goindex/docker-compose.yml` |
| Environment file | `/mnt/cache/appdata/goindex/.env` (permissions: `0600`) |
| Docker network | `nzb` (external, shared with other containers) |
| Published port | `8093` → container `8080` |
| Base URL | `http://192.168.1.51:8093` |
| Migration backup | `/mnt/user/backups/PostgreSQL/goindex-migration/` |

## Prerequisites

- A PostgreSQL 17+ container running on the `nzb` network with a `goindex` database and user
- A Usenet provider account with NNTP credentials
- `docker` and `docker compose` on the Unraid host

## Initial deployment

```sh
mkdir -p /mnt/cache/appdata/goindex
cd /mnt/cache/appdata/goindex
```

Create `.env` with all required variables:

```sh
cat > .env << 'EOF'
GOINDEX_DB_HOST=NZB-postgresql17
GOINDEX_DB_PORT=5432
GOINDEX_DB_USER=goindex
GOINDEX_DB_PASSWORD=<db-password>
GOINDEX_DB_NAME=goindex
GOINDEX_DB_SSL_MODE=disable
GOINDEX_SERVER_LISTEN_ADDR=:8080
GOINDEX_SERVER_BASE_URL=http://192.168.1.51:8093
GOINDEX_AUTH_JWT_SECRET=<long-random-hex-string>
GOINDEX_NNTP_HOST=news.provider.com
GOINDEX_NNTP_PORT=563
GOINDEX_NNTP_TLS=true
GOINDEX_NNTP_USERNAME=<username>
GOINDEX_NNTP_PASSWORD=<password>
GOINDEX_NNTP_MAX_CONNS=20
GOINDEX_SCAN_GROUPS=alt.binaries.boneless
GOINDEX_SCAN_INTERVAL=30m
GOINDEX_LOG_LEVEL=info
GOINDEX_PORT=8093
EOF
chmod 600 .env
```

Create `docker-compose.yml`:

```yaml
name: goindex
services:
  goindex:
    container_name: goindex
    image: goindex:latest
    restart: unless-stopped
    ports:
      - "${GOINDEX_PORT:-8093}:8080"
    networks:
      - nzb
    env_file: .env
    healthcheck:
      test: ["CMD", "/usr/local/bin/goindex", "healthcheck"]
      interval: 30s
      timeout: 10s
      retries: 5
      start_period: 30s

networks:
  nzb:
    external: true
```

> **Port conflicts:** If `8092` or other ports are occupied, choose the next available port and update both `GOINDEX_PORT` and `GOINDEX_SERVER_BASE_URL` in `.env` and the Compose published port.

Start the container:

```sh
docker compose up -d
```

## Upgrade

```sh
cd /mnt/cache/appdata/goindex
docker compose pull
docker compose up -d --force-recreate
# Verify health
docker inspect goindex --format '{{.State.Health.Status}}'
curl -fsS http://127.0.0.1:8093/api/v1/health
```

## Restart

```sh
docker compose restart
# or
docker compose up -d --force-recreate
```

## Database backup

Use the project''s `scripts/backup.sh` adapted for Unraid paths:

```sh
DB_HOST=NZB-postgresql17 DB_USER=goindex DB_NAME=goindex \
  ./scripts/backup.sh /mnt/user/backups/PostgreSQL/goindex/
```

Or run directly:

```sh
docker exec NZB-postgresql17 pg_dump -U goindex -Fc -f /backups/goindex-$(date +%Y%m%d-%H%M%S).dump goindex
```

Schedule via Unraid''s User Scripts plugin.

## Database restore

> **Destructive:** overwrites the current database. Stop the app first.

```sh
docker compose stop goindex
docker exec -i NZB-postgresql17 pg_restore --clean --if-exists --no-owner -U goindex -d goindex < /path/to/dump.dump
docker compose start goindex
```

## Logs

```sh
docker logs goindex
docker logs --tail 200 -f goindex
```

The admin UI also provides a live log view under System > Logs.

## JWT secret rotation

1. Update `GOINDEX_AUTH_JWT_SECRET` in `.env`
2. Restart the container: `docker compose restart`
3. All existing sessions are invalidated; users must log in again

## Networking

The container joins the `nzb` bridge network for connectivity to `NZB-postgresql17`. The published port `8093` is the LAN-accessible endpoint. All NZB download links use the configured `base_url`.
