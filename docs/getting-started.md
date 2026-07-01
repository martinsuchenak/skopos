# Getting Started

## Prerequisites

- **Go** 1.26+
- **Bun** (for the frontend build)
- **Task** (task runner — [install](https://taskfile.dev))

## Build from source

```bash
git clone https://github.com/martinsuchenak/skopos
cd skopos
task build-local    # builds frontend + Go binary → bin/skopos
```

## Docker

```bash
docker build -t skopos .
docker run -p 8080:8080 -v skopos-data:/app/data skopos serve
```

See [Deployment → Docker](deployment/docker.md) for details.

## First run

```bash
./bin/skopos serve
```

Open `http://localhost:8080`. The dashboard has three views:

- **Sessions** — live agent status (progress bars, events timeline, stuck detection).
- **Blackboard** — shared knowledge entries grouped by type (bugs, findings, decisions, …).
- **Plans** — to-do lists with item dependencies and auto-blocking.

All data is stored in `skopos.db` (SQLite, WAL mode). No external database needed.

## Configuration

```bash
cp skopos-config.example.toml skopos-config.toml   # optional
```

The config file is optional — skopos runs on flag defaults + env vars without it. See [Configuration](configuration.md) for all options.

Key flags:
```bash
skopos serve --api-key mysecret    # require API key for writes
skopos serve --log-level debug     # debug logging
```

## Connecting an agent

```bash
# Wire the MCP server connection into an agent's config
skopos install --agent claude-code

# Remote skopos with auth
skopos install --agent all --url https://skopos.example.com/mcp --api-key "$SKOPOS_API_KEY"
```

This merges the MCP server entry (with auth header) into the agent's config file. For per-agent manual steps and behavioral snippets, see [Integration guides](../integrations/).

## Agent-builder artifacts

To equip agents with skopos skills/commands/rules (compiled from one source):

```bash
agent-builder compile docs/integrations/agent-builder/ab-src
agent-builder install
```

See [agent-builder README](../integrations/agent-builder/README.md).
