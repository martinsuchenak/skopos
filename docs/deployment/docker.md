# Docker

## Build

```bash
docker build -t skopos .
```

The Dockerfile is a multi-stage build:
1. **Frontend stage** — Bun installs + builds the dashboard (`web/dist/`).
2. **Go stage** — compiles the binary with the frontend embedded (via `go:embed`).
3. **Runtime stage** — Alpine, runs as non-root `skopos` user, includes `ca-certificates` + `wget` (for healthcheck).

## Run

```bash
docker run -d \
  --name skopos \
  -p 8080:8080 \
  -v skopos-data:/app \
  -e SKOPOS_API_KEY=mysecret \
  skopos serve
```

The SQLite database (`skopos.db`) is written to `/app`. Mount a volume to persist data across restarts.

## Environment variables

All [configuration options](../configuration.md) can be set via env vars:

```bash
-e SERVER_PORT=9090
-e SKOPOS_API_KEY=mysecret
-e LOG_LEVEL=debug
-e DATABASE_PATH=/app/data/skopos.db
-e HEALTH_STUCK_THRESHOLD=10
-e CLEANUP_RETENTION_DAYS=14
```

## Health check

The Dockerfile includes a built-in healthcheck:
```dockerfile
HEALTHCHECK CMD wget -qO- http://127.0.0.1:8080/health || exit 1
```

Docker reports `healthy` when the `/health` endpoint responds 200.

## Ports

| Port | Service |
|------|---------|
| 8080 | HTTP (REST API, dashboard, MCP at `/mcp`, SSE at `/api/events/stream`) |

Only one port — MCP, SSE, and the dashboard are all served on the main HTTP port.
