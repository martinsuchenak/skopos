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
| `--server-host` | `SERVER_HOST` | `server.host` | `127.0.0.1` | HTTP listen host (set `0.0.0.0` to listen on all interfaces) |
| `--server-port` | `SERVER_PORT` | `server.port` | `8080` | HTTP listen port (REST, MCP at `/mcp`, dashboard, SSE) |
| `--database-path` | `DATABASE_PATH` | `database.path` | `skopos.db` | SQLite database file path |
| `--api-key` | `SKOPOS_API_KEY` | `auth.api_key` | (empty = auth disabled, loopback only) | API key; when set, required by every endpoint (REST, MCP, SSE) |
| `--health-stuck-threshold` | `HEALTH_STUCK_THRESHOLD` | `health.stuck_threshold_minutes` | `15` | Minutes before an active agent is marked stuck (0 disables) |
| `--cleanup-retention-days` | `CLEANUP_RETENTION_DAYS` | `cleanup.retention_days` | `30` | Days to retain data (0 disables cleanup) |
| `--index-dir` | `SKOPOS_INDEX_DIR` | `codeindex.dir` | `indexes` | Directory for per-workspace code index databases |
| `--embeddings-url` | `SKOPOS_EMBEDDINGS_URL` | `codeindex.embeddings.url` | (empty = disabled) | OpenAI-compatible `/v1/embeddings` endpoint for semantic code search (local Ollama works) |
| `--embeddings-model` | `SKOPOS_EMBEDDINGS_MODEL` | `codeindex.embeddings.model` | (empty) | Embedding model name (required with `--embeddings-url`) |
| `--embeddings-api-key` | `SKOPOS_EMBEDDINGS_API_KEY` | `codeindex.embeddings.api_key` | (empty) | API key for the embeddings endpoint (not needed locally) |
| `--vector-store` | `SKOPOS_VECTOR_STORE` | `codeindex.embeddings.vector_store` | `sqlite` | Vector backend: `sqlite` (embedded brute force) or `qdrant` (external, monorepo scale) |
| `--qdrant-url` | `SKOPOS_QDRANT_URL` | `codeindex.embeddings.qdrant_url` | (empty) | Qdrant REST address when `--vector-store=qdrant` (e.g. `http://localhost:6333`) |
| `--qdrant-api-key` | `SKOPOS_QDRANT_API_KEY` | `codeindex.embeddings.qdrant_api_key` | (empty) | Qdrant API key when required |

## Log levels

```bash
skopos serve --log-level debug      # see every HTTP request
skopos serve --log-level trace      # maximum verbosity
LOG_LEVEL=debug skopos serve        # via env var
```

With `debug`, every HTTP request is logged (method, path, status, duration) through the single paularlott logger. All log output is consistent (one format, one writer).

## Authentication

When `api_key` is set, every endpoint (reads, writes, MCP, SSE) requires the key via:

```
Authorization: Bearer mysecret
```

When empty (default), authentication is disabled — all endpoints are open. A startup warning is logged.

## Database

SQLite with WAL journal mode, foreign keys enforced per-connection (via DSN pragma), and a bounded connection pool. Migrations run automatically on every `serve` (idempotent `CREATE TABLE IF NOT EXISTS`). The schema lives in `internal/db/schema.sql`.
