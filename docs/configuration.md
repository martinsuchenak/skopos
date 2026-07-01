# Configuration

Skopos loads configuration from (in priority order): **CLI flags** > **environment variables** > **TOML config file** > **defaults**.

## Config file

```bash
cp skopos-config.example.toml skopos-config.toml
```

Optional — if absent, skopos runs on defaults. The file is gitignored (may contain secrets).

## All options

| Flag | Env var | Config path | Default | Description |
|------|---------|-------------|---------|-------------|
| `--config` | `CONFIG_FILE` | — | `skopos-config.toml` | Path to config file |
| `--log-level` | `LOG_LEVEL` | `log.level` | `info` | `trace`, `debug`, `info`, `warn`, `error` |
| `--log-format` | `LOG_FORMAT` | `log.format` | `text` | `text` or `json` |
| `--server-host` | `SERVER_HOST` | `server.host` | `0.0.0.0` | HTTP listen host |
| `--server-port` | `SERVER_PORT` | `server.port` | `8080` | HTTP listen port (REST, MCP at `/mcp`, dashboard, SSE) |
| `--database-path` | `DATABASE_PATH` | `database.path` | `skopos.db` | SQLite database file path |
| `--api-key` | `SKOPOS_API_KEY` | `auth.api_key` | (empty = open) | API key for write endpoints + MCP |
| `--health-stuck-threshold` | `HEALTH_STUCK_THRESHOLD` | `health.stuck_threshold_minutes` | `15` | Minutes before an active agent is marked stuck |
| `--cleanup-retention-days` | `CLEANUP_RETENTION_DAYS` | `cleanup.retention_days` | `30` | Days to retain data (0 disables cleanup) |

## Log levels

```bash
skopos serve --log-level debug      # see every HTTP request
skopos serve --log-level trace      # maximum verbosity
LOG_LEVEL=debug skopos serve        # via env var
```

With `debug`, every HTTP request is logged (method, path, status, duration) through the single paularlott logger. All log output is consistent (one format, one writer).

## Authentication

When `api_key` is set, all write endpoints (POST/PATCH/DELETE) and the MCP endpoint require the key via:

```
Authorization: Bearer mysecret
```

When empty (default), authentication is disabled — all endpoints are open. A startup warning is logged.

## Database

SQLite with WAL journal mode, foreign keys enforced per-connection (via DSN pragma), and a bounded connection pool. Migrations run automatically on every `serve` (idempotent `CREATE TABLE IF NOT EXISTS`). The schema lives in `internal/db/schema.sql`.
