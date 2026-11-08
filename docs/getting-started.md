# Getting Started

## Install

On Linux or macOS:

```bash
curl -fsSL https://github.com/martinsuchenak/skopos/releases/latest/download/install.sh | sh
```

- **What it does:** downloads the release binary for your OS and CPU,
  checks it against the release's `checksums.txt`, and installs it as
  `~/.local/bin/skopos`. When `cosign` is installed, it also verifies
  `checksums.txt` against the release's Sigstore signature, and stops if it
  doesn't match.
- **Options:** `--version 0.23.2` (or `SKOPOS_VERSION`) for a given
  release; `--dir DIR` (or `SKOPOS_INSTALL_DIR`) for another directory.
  Pass them after `sh -s --`: `curl -fsSL …/install.sh | sh -s -- --version 0.23.2`.
- **Upgrade:** run it again. A worker running this binary upgrades itself to
  the server's release, since the binary sits in a directory the worker can
  write to ([Self-upgrade](deployment/worker.md#self-healing)).
- **After it:** it says if `~/.local/bin` isn't on your PATH, if another
  `skopos` (say, the Homebrew cask) comes first, and if a worker service runs
  another binary (re-run `skopos worker install` to point it at this one).
- **A worker too:** add `--worker --server-url … --workspace …` and it goes
  on to set the worker up and start its service
  ([Deploying a worker](deployment/worker.md#quick-start)).

Other ways:

- **Homebrew:** `brew install --cask martinsuchenak/tap/skopos`. A worker
  installed this way upgrades itself through `brew`.
- **A release download:** `skopos_<os>_<arch>` (Windows: `.exe`) or an
  archive from the [releases](https://github.com/martinsuchenak/skopos/releases).
- **Docker:** `ghcr.io/martinsuchenak/skopos` (below).

## Build from source

Needs:

- **Go** 1.26+
- **Bun** (for the frontend build)
- **Task** (task runner — [install](https://taskfile.dev))


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

Open `http://localhost:8080`. The dashboard has four views:

- **Sessions** — live agent status (progress bars, events timeline, stuck detection).
- **Blackboard** — shared knowledge entries grouped by type (bugs, findings, decisions, …).
- **Plans** — to-do lists with item dependencies and auto-blocking.
- **Index** — code index state per workspace: branches, freshness, symbol counts.

A workspace filter in the sidebar scopes the views to one project; workspaces
are registered automatically as agents and CLI commands report in.

All data is stored in `skopos.db` (SQLite, WAL mode). No external database needed.

## Configuration

```bash
cp skopos-config.example.toml skopos-config.toml   # optional
```

The config file is optional — skopos runs on flag defaults + env vars without it. See [Configuration](configuration.md) for all options.

Key flags:
```bash
skopos serve --api-key mysecret    # root key: required on every endpoint (REST, MCP, SSE)

# The --api-key above is the ROOT key (full access, manages keys/workspaces).
# For agents and machines, mint scoped keys instead — each limited to chosen
# workspaces: `skopos key create --name zcode-laptop --workspace github.com/me/repo`
# (see docs/concepts/api-keys.md).
skopos serve --log-level debug     # debug logging
```

## Connecting an agent

```bash
# Wire the MCP server connection into an agent's config
skopos install --agent claude-code

# Remote skopos with auth
skopos install --agent all --url https://skopos.example.com/mcp --api-key "$SKOPOS_API_KEY"
```

This merges the MCP server entry (with auth header) into the agent's config file. For per-agent manual steps and behavioral snippets, see [Integration guides](integrations/).

## Next: the agent pipeline

To have agents plan and implement inbox items for you — with workers, stage
routing and approvals — follow [the pipeline tutorial](guides/pipeline.md).

## Agent-builder artifacts

To equip agents with skopos skills/commands/rules (compiled from one source):

```bash
agent-builder compile docs/integrations/agent-builder/ab-src
agent-builder install
```

See [agent-builder README](integrations/agent-builder/README.md).
