# skopos

A coordination dashboard for AI coding agents — shared memory (blackboard), plans with dependencies, real-time status, and live updates via SSE. Single binary, SQLite, no external dependencies.

## Features

| Feature | Status |
|---------|--------|
| CLI | ✅ `serve`, `report`, `blackboard`, `plan`, `workspace`, `index`, `search`, `symbol`, `who-calls`, `call-tree`, `impact`, `outline`, `dead-code`, `cycles`, `branch-diff`, `install`, `cleanup`, `init`, `completion` |
| REST API | ✅ Sessions, blackboard, plans, workspaces |
| MCP | ✅ 23 tools at `/mcp` (same port as HTTP), incl. code-index queries |
| Dashboard | ✅ Dark/light/system theme, sidebar nav, modals, SSE live updates |
| Real-time | ✅ SSE at `/api/events/stream` |
| Database | ✅ SQLite (WAL, FK-enforced, transactional) |
| Auth | ✅ API key (Bearer); when set it gates every endpoint (REST reads/writes, MCP, SSE) |
| Agent integration | ✅ `skopos install` for Claude Code, Codex, Gemini, Copilot, Kiro, opencode |
| Code index | ✅ Central, branch-aware symbol/call-graph index (all languages, optional semantic search) |
| Docker | ✅ |
| Nomad | ✅ |

## Quick start

```bash
task build-local          # build the binary
./bin/skopos serve        # start on :8080 (HTTP + MCP + dashboard)
```

Open `http://localhost:8080`.

## Connect an agent

```bash
skopos install --agent claude-code          # local
skopos install --agent all --api-key "$SKOPOS_API_KEY"  # remote + auth
```

See [Agent integration](docs/getting-started.md#connecting-an-agent) and [Integration guides](docs/integrations/).

## Documentation

| Topic | Link |
|-------|------|
| Getting started (install, first run, tour) | [docs/getting-started.md](docs/getting-started.md) |
| Configuration (flags, env vars, log levels) | [docs/configuration.md](docs/configuration.md) |
| Blackboard (memory, scopes, search) | [docs/concepts/blackboard.md](docs/concepts/blackboard.md) |
| Code index (symbols, call graph, impact) | [docs/concepts/code-index.md](docs/concepts/code-index.md) |
| Plans (items, dependencies, auto-block) | [docs/concepts/plans.md](docs/concepts/plans.md) |
| Status (reporting, health checker) | [docs/concepts/status.md](docs/concepts/status.md) |
| Workspaces (scoping, registry) | [docs/concepts/workspaces.md](docs/concepts/workspaces.md) |
| Events (SSE, real-time) | [docs/concepts/events.md](docs/concepts/events.md) |
| Deployment (Docker, Nomad) | [docs/deployment/](docs/deployment/) |
| Integration guides (6 agents) | [docs/integrations/](docs/integrations/) |
| API spec (OpenAPI) | [openapi.yaml](openapi.yaml) |

## License

MIT — see [LICENSE](LICENSE).
