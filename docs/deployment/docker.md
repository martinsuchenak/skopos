# Docker

## Release image

Every release publishes a multi-arch image (`linux/amd64`, `linux/arm64`),
signed with cosign:

```bash
docker pull ghcr.io/martinsuchenak/skopos:0.21.0   # or 0.21, or latest
```

Use it in place of `skopos` in the examples below. Verify the signature with
`cosign verify ghcr.io/martinsuchenak/skopos:<version>
--certificate-identity-regexp 'https://github.com/martinsuchenak/skopos/'
--certificate-oidc-issuer https://token.actions.githubusercontent.com`.

## Build

```bash
task docker   # tags skopos:<git describe>, with the version and commit stamped in
docker build --build-arg VERSION=$(git describe --tags --always) --build-arg COMMIT=$(git rev-parse --short HEAD) -t skopos .
```

The Dockerfile is a multi-stage build:
1. **Frontend stage** — Bun installs + builds the dashboard (`web/dist/`).
2. **Go stage** — compiles the binary with the frontend embedded (via `go:embed`).
3. **Runtime stage** — Alpine, runs as the non-root `skopos` user (UID 1000), includes `ca-certificates`, `wget` (for the healthcheck) and `git` (for the code index refresher).

The binary is at `/usr/local/bin/skopos`, outside `/app`, so the volume
mounted there never shadows it with an older copy (images before 0.21.0 kept
it in `/app`). `docker exec <container> skopos …` runs CLI commands.

## Run

```bash
docker run -d \
  --name skopos \
  -p 8080:8080 \
  -v skopos-data:/app \
  -e SKOPOS_API_KEY=mysecret \
  skopos serve
```

The SQLite database (`skopos.db`) is written to `/app`. Mount a volume to persist data across restarts. Use a local Docker volume or a host directory on local disk, not an NFS or SMB mount: SQLite over a network filesystem is slow and can corrupt (skopos warns at startup). For shared storage, see the Litestream setup in the [Nomad guide](nomad.md).

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
