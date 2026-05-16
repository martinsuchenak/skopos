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

## Blackboard

Codex can use blackboard via MCP tools (if MCP is connected) or the CLI.

**Via MCP** — tools appear as `skopos__blackboard_write` and `skopos__blackboard_read` once Codex connects to the MCP server.

**Via CLI** — call `skopos blackboard` commands from shell steps in your AGENTS.md:

```bash
# Read prior findings at session start
skopos blackboard read --branch "$(git branch --show-current)" || true

# Write a finding during work
skopos blackboard write \
  --scope branch --branch "$(git branch --show-current)" \
  --type finding --title "..." --content "..." \
  --agent-id "codex-$(hostname -s)" \
  ${SKOPOS_API_KEY:+--api-key "$SKOPOS_API_KEY"} || true
```

Entry types: `finding`, `decision`, `bug`, `debt`, `warning`, `context`

`bug` and `debt` entries are always visible across all branches. Entries appear in the Skopos dashboard under the **Blackboard** tab.

## Plans

Codex can use plans via MCP tools (if MCP is connected) or the CLI.

**Via MCP** — tools appear as `skopos__plan_create`, `skopos__plan_read`, `skopos__plan_add_item`, and `skopos__plan_update_item`.

**Via CLI** — call `skopos plan` commands from shell steps in your AGENTS.md:

```bash
# Create a plan at task start
skopos plan create --name "Task name" --branch "$(git branch --show-current)" \
  --agent-id "codex-$(hostname -s)" \
  ${SKOPOS_API_KEY:+--api-key "$SKOPOS_API_KEY"} || true

# Add items
skopos plan item add --plan-id "PLAN_ID" --title "Item title" \
  ${SKOPOS_API_KEY:+--api-key "$SKOPOS_API_KEY"} || true

# Mark items done
skopos plan item done --plan-id "PLAN_ID" --item-id "ITEM_ID" \
  ${SKOPOS_API_KEY:+--api-key "$SKOPOS_API_KEY"} || true
```

Plans appear in the Skopos dashboard under the **Plans** tab.

## Session IDs

Same resolution as other agents — see `shared/skopos-session.sh`. Set `$SKOPOS_SESSION_ID` to share a session across agents.
