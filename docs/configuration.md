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
| `--insecure-no-api-key` | `SKOPOS_INSECURE_NO_API_KEY` | — | `false` | Allow running without an API key on a non-loopback bind (authentication stays disabled — know what you are exposing) |
| `--health-stuck-threshold` | `HEALTH_STUCK_THRESHOLD` | `health.stuck_threshold_minutes` | `15` | Minutes before an active agent is marked stuck (0 disables) |
| `--cleanup-retention-days` | `CLEANUP_RETENTION_DAYS` | `cleanup.retention_days` | `30` | Days to retain data (0 disables cleanup) |
| `--index-dir` | `SKOPOS_INDEX_DIR` | `codeindex.dir` | `.skopos/indexes` | Directory for per-workspace code index databases |
| `--refresh-interval` | `SKOPOS_REFRESH_INTERVAL` | `codeindex.refresh_interval` | `0` (off) | Poll registered `git_url` workspaces for server-side refresh (e.g. `30m`, `1h`); CI can also POST the refresh endpoint on merge |
| `--embeddings-url` | `SKOPOS_EMBEDDINGS_URL` | `codeindex.embeddings.url` | (empty = disabled) | OpenAI-compatible `/v1/embeddings` endpoint for semantic code search (local Ollama works) |
| `--embeddings-model` | `SKOPOS_EMBEDDINGS_MODEL` | `codeindex.embeddings.model` | (empty) | Embedding model name (required with `--embeddings-url`) |
| `--embeddings-api-key` | `SKOPOS_EMBEDDINGS_API_KEY` | `codeindex.embeddings.api_key` | (empty) | API key for the embeddings endpoint (not needed locally) |
| `--vector-store` | `SKOPOS_VECTOR_STORE` | `codeindex.embeddings.vector_store` | `sqlite` | Vector backend: `sqlite` (embedded brute force) or `qdrant` (external, monorepo scale) |
| `--qdrant-url` | `SKOPOS_QDRANT_URL` | `codeindex.embeddings.qdrant_url` | (empty) | Qdrant REST address when `--vector-store=qdrant` (e.g. `http://localhost:6333`) |
| `--qdrant-api-key` | `SKOPOS_QDRANT_API_KEY` | `codeindex.embeddings.qdrant_api_key` | (empty) | Qdrant API key when required |

### Client (written by `skopos setup`)

The `[client]` section configures CLI commands (`index push`, queries) to
talk to a remote server without passing flags. `skopos setup` writes it
automatically; flags and `SKOPOS_SERVER_URL`/`SKOPOS_API_KEY` override it.

| Config path | Env var | Default | Description |
|-------------|---------|---------|-------------|
| `client.server_url` | `SKOPOS_SERVER_URL` | (empty = local index) | Remote skopos URL for index/query commands |
| `client.api_key` | `SKOPOS_API_KEY` | (empty) | API key for the remote server |

### Worker

The `[worker]` section configures the execution plane (`skopos worker`).
Scaffold it with `skopos worker init` (which also writes a `skopos-worker.env`
for secrets). See [deployment/worker.md](deployment/worker.md) for the full
walkthrough.

| Config path | Flag | Default | Description |
|-------------|------|---------|-------------|
| `worker.id` | `--worker-id` | `worker-<hostname>` | Worker identity — must keep the `worker-` prefix |
| `worker.state_dir` | `--state-dir` | `~/.local/state/skopos-worker` | Worktrees, logs, session state |
| `worker.runners` | — | `["claude"]` | Runners this worker offers (must cover the workspaces' stage preferences) |
| `worker.workspaces` | `--workspace` | (from repos) | Workspace IDs this worker serves |
| `worker.poll_every` | — | `30s` | Idle claim interval |
| `worker.repos."<workspace-id>"` | — | — | Per-workspace local clone: `path`, `default_branch`, `branch_prefix`, `setup`, `setup_env`, `checks`, `protected_branches`, `profile`, `stages.<stage>`, and `push.{kind,remote_url,token_env}` |
| `worker.accounts` | — | — | Credential pool: `name`, `runner` (`claude`, `opencode`, or a `runner_config` name), `priority`, `model`, `env` (injected into agent runs only) |
| `worker.runner_config.<name>` | — | — | Agent CLI per runner name: `kind` (`claude` / `opencode` / `command`), `bin`, `config` (opencode base config: providers/models), `command` (argv template for any other CLI — unverified, wrapper-only) |
| `worker.claude_bin` | — | `claude` | Claude Code binary (or `runner_config.claude.bin`) — must support `--setting-sources`, `--restricted`, `--strict-mcp-config` (checked at start) |
| `worker.plan_timeout` / `worker.impl_timeout` | — | `10m` / `90m` | Agent run timeouts (review/qa use the plan timeout) |
| `worker.mcp_config` | — | — | JSON file with the MCP servers pipeline runs get (Claude Code `mcpServers` shape); required when `mcp_servers` is set |
| `worker.mcp_servers` | — | — | MCP tool prefixes allowed in plan/implement runs, e.g. `["mcp__skopos"]` |
| `worker.mcp_deny` | — | Fortix production-write tools | MCP tools denied in every run (added to the built-in git/network denials) |
| `worker.profiles.<name>.<stage>` | — | — | Operator-supplied stage executor (`plan`/`implement`/`review`/`qa`): `wrapper`, `settings`, `tools`, `env` — see [deployment/worker.md](deployment/worker.md#containing-agent-runs) |
| — | `--env-file` | `<config-dir>/skopos-worker.env` when present | `KEY=VALUE` secrets loaded before the config (real environment wins) |

## Slack relay (server)

Enabled when all three `SKOPOS_SLACK_*` variables are set on `skopos serve`;
absent = disabled. Socket Mode (outbound websocket — no public URL needed).
The relay posts one thread per item (the `inbox_items.slack_thread_ts` column
persists the binding) on every gate (plan ready / blocked / branch ready /
done) in the bot's DM with the configured user, and applies that user's DM
replies onto the approver-gated workflow actions, audit-logged `via: slack`.
Only direct messages from `SKOPOS_SLACK_USER_ID` are read — other users and
channels the app can see are ignored and cannot change where messages go.
The Slack app needs the `chat:write`, `reactions:write` and `im:history` bot
scopes and the `message.im` event subscription.

| Env var | Meaning |
|---------|---------|
| `SKOPOS_SLACK_BOT_TOKEN` | Bot token (`xoxb-…`) — posting, reactions |
| `SKOPOS_SLACK_APP_TOKEN` | App-level token (`xapp-…`) — Socket Mode |
| `SKOPOS_SLACK_USER_ID` | The only user whose replies are honored |
| `SKOPOS_SLACK_DEFAULT_WORKSPACE` | Workspace for `new: <what>` capture (unset disables capture) |

Commands (replies in an item's thread): `approve` [`: <notes>`], `revise:
<notes>`, `changes: <notes>`, `done`, `retry: <answer>`, `reject`. A command is
the bare word or `word: <text>` on its own — "done with lunch" is not `done`
(a reply that leads with a command word gets a syntax hint instead).
Top-level: `new: <what to do>` or `new <workspace>: <what>` (exact id or a
unique `/suffix`), `help`. Non-approving replies (`revise`, `changes`,
`retry`, `reject`) that arrive mid-run are buffered and applied when the run
finishes (in-memory; a restart drops unapplied ones); `approve` and `done`
are refused mid-run — they answer the gate message they approve.

## Agent identity (all surfaces)

These environment variables affect how agents identify themselves — to the
server (MCP/REST) and to CLI commands:

| Env var | Default | Description |
|---------|---------|-------------|
| `SKOPOS_SESSION_ID` | auto-derived (per workspace per day, or `.skopos-session` file in the workspace root) | Share one session across multiple agents working on the same task |
| `SKOPOS_AGENT_ID` | `<command>-<hostname>` | Stable agent id for `skopos report` and hook scripts |

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

## API keys

Besides the root `api_key`, the server supports scoped API keys stored in
the database — see [concepts/api-keys.md](concepts/api-keys.md). The root
key stays in configuration and is the only key that can mint, edit, revoke,
or delete others; generate a strong one with `skopos key generate-root`.
`last_used_at` is updated at most once per 5 minutes per key.

## MCP lean tools

`[mcp] lean_tools = true` (or `--mcp-lean-tools` / `SKOPOS_MCP_LEAN_TOOLS`)
hides non-core MCP tools from `tools/list`, keeping only the workhorses
natively visible (code_search, code_find, code_symbol, code_callers,
code_impact, skopos_context, blackboard_read/write, report_status). Hidden
tools remain fully callable via the `tool_search` / `execute_tool`
meta-tools the server exposes automatically. This roughly halves the
per-step schema weight agents re-send every turn — the measured step-level
token cost in the index-effectiveness benchmarks. Default: off (all tools
listed).
