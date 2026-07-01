# skopos

A coordination dashboard for AI coding agents — shared memory (blackboard), plans with dependencies, real-time status, and live updates via SSE. Single binary, SQLite, no external dependencies.

## Features

| Feature | Status |
|---------|--------|
| CLI | ✅ `serve`, `report`, `blackboard`, `plan`, `workspace`, `install`, `cleanup` |
| REST API | ✅ Sessions, blackboard, plans, workspaces |
| MCP | ✅ 12 tools at `/mcp` (same port as HTTP) |
| Dashboard | ✅ Dark/light/system theme, sidebar nav, modals, SSE live updates |
| Real-time | ✅ SSE at `/api/events/stream` |
| Database | ✅ SQLite (WAL, FK-enforced, transactional) |
| Auth | ✅ API key (X-API-Key or Bearer), write-only |
| Agent integration | ✅ `skopos install` for Claude Code, Codex, Gemini, Copilot, Kiro, opencode |
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
| Plans (items, dependencies, auto-block) | [docs/concepts/plans.md](docs/concepts/plans.md) |
| Status (reporting, health checker) | [docs/concepts/status.md](docs/concepts/status.md) |
| Workspaces (scoping, registry) | [docs/concepts/workspaces.md](docs/concepts/workspaces.md) |
| Events (SSE, real-time) | [docs/concepts/events.md](docs/concepts/events.md) |
| Deployment (Docker, Nomad) | [docs/deployment/](docs/deployment/) |
| Integration guides (6 agents) | [docs/integrations/](docs/integrations/) |
| API spec (OpenAPI) | [openapi.yaml](openapi.yaml) |

## License

MIT — see [LICENSE](LICENSE).
