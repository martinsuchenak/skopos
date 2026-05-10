# Codex → Skopos Integration

## Prerequisites

- Skopos server running: `skopos serve` (REST on :8080, MCP on :9000)
- `skopos` binary in PATH
- Codex CLI installed

## Step 1: Apply MCP config

Add the `skopos` entry from `config-snippet.toml` to `~/.codex/config.toml`:

```toml
[mcp_servers.skopos]
enabled = true
url = "http://localhost:9000/mcp"
```

## Step 2: Wire lifecycle hooks via AGENTS.md

Codex reads `AGENTS.md` files for behavioral instructions. Append `AGENTS-snippet.md` to your project's `AGENTS.md`:

```bash
cat docs/integrations/codex/AGENTS-snippet.md >> AGENTS.md
```

This instructs Codex to call `skopos report` at session start, success, and failure.

Set environment variables (add to `~/.zshrc` or `~/.bashrc`):

```bash
export SKOPOS_API_KEY=your-key-here
export SKOPOS_SERVER_URL=http://localhost:8080
```

## Step 3: Verify

Start a Codex session. Open `http://localhost:8080`. You should see a `codex-<hostname>` agent appear.

## Session IDs

Same resolution as other agents — see `shared/skopos-session.sh`. Set `$SKOPOS_SESSION_ID` to share a session across agents.
