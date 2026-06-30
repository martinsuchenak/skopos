# GitHub Copilot → Skopos Integration

Two layers: MCP (rich voluntary reporting via Copilot Chat) + VS Code workspace task (automatic session start on folder open).

## Prerequisites

- Skopos server running: `skopos serve` (HTTP on :8080, MCP at /mcp)
- `skopos` binary in PATH
- GitHub Copilot extension installed in VS Code

> **Quick install:** `skopos install --agent github-copilot [--url ...] [--api-key "$SKOPOS_API_KEY"]` does this for you — it merges the MCP config into VS Code's `mcp.json` and appends `.github/copilot-instructions.md` (idempotent, backs up existing config). The manual steps below are the fallback.

## Step 1: Apply MCP config

Add the `skopos` entry from `mcp-snippet.json` into VS Code's global MCP config:

**File:** `~/Library/Application Support/Code/User/mcp.json`

```json
"skopos": {
  "type": "http",
  "url": "http://localhost:8080/mcp",
  "headers": {
    "Authorization": "Bearer ${SKOPOS_API_KEY}"
  }
}
```

Add it under the existing `"servers"` object. Create the file if it doesn't exist:

```json
{
  "servers": {
    "skopos": {
      "type": "http",
      "url": "http://localhost:8080/mcp",
      "headers": {
        "Authorization": "Bearer ${SKOPOS_API_KEY}"
      }
    }
  }
}
```

> **Auth:** The `Authorization` header is only required when `auth.api_key` is set on the server. If your VS Code version doesn't expand `${SKOPOS_API_KEY}`, paste the literal key.

Alternatively, for workspace-level MCP config, add to `.vscode/mcp.json` in your project using the same format.

After saving, reload VS Code. The `skopos__report_status` tool will appear in Copilot Chat's tool list.

## Step 2: Wire lifecycle hooks

### Automatic — VS Code workspace task (session start)

Merge `vscode-tasks-snippet.json` into `.vscode/tasks.json` in your workspace. This fires `skopos report` automatically when VS Code opens the folder:

```bash
# If .vscode/tasks.json doesn't exist yet:
mkdir -p .vscode
cp docs/integrations/github-copilot/vscode-tasks-snippet.json .vscode/tasks.json

# If it already exists, merge the task into the existing "tasks" array manually.
```

> **Note:** VS Code requires user confirmation the first time an auto-run task fires. Click "Allow" in the notification.

### Behavioral — Copilot chat instructions

Append `copilot-instructions-snippet.md` to your project's `.github/copilot-instructions.md`:

```bash
mkdir -p .github
cat docs/integrations/github-copilot/copilot-instructions-snippet.md >> .github/copilot-instructions.md
```

This instructs Copilot to call `skopos__report_status` at session start, during work (with granular status), and at completion.

Set environment variables (add to `~/.zshrc` or `~/.bashrc`):

```bash
export SKOPOS_API_KEY=your-key-here
export SKOPOS_SERVER_URL=http://localhost:8080
```

## Step 3: Verify

1. Open a workspace in VS Code with the task configured. The workspace-task fires `skopos report` on folder open.
2. Open Copilot Chat and start a conversation. Copilot will call `skopos__report_status` per the instructions in `.github/copilot-instructions.md`.
3. Open `http://localhost:8080` — you should see a `github-copilot-<hostname>` agent with status `running`.

## Session IDs

GitHub Copilot does not manage `session_id` automatically. Options:

- Set `$SKOPOS_SESSION_ID` in your shell before opening VS Code, or
- Let the workspace task generate a stable ID via the shared helper (not wired by default), or
- Leave unset — the server creates a new session per run, grouped by `agent_id`

## Blackboard

Once MCP is connected, Copilot Chat has access to `skopos__blackboard_write` and `skopos__blackboard_read` tools.

Read the Knowledge Bundle at the start of a task to load prior context for the current branch. Write entries when you find bugs, make decisions, or want to leave a note for the next agent session. `bug` and `debt` entries are always visible to all agents regardless of branch.

Entries appear in the Skopos dashboard under the **Blackboard** tab at `http://localhost:8080`.

## Plans

Once MCP is connected, Copilot Chat has access to `skopos__plan_create`, `skopos__plan_read`, `skopos__plan_add_item`, and `skopos__plan_update_item` tools.

Create a plan at the start of a multi-step task, add items, and update their status as you work. Plans appear in the Skopos dashboard under the **Plans** tab at `http://localhost:8080`.

## Notes

- The MCP tool name in Copilot Chat appears as `skopos__report_status` (server name + double underscore + tool name)
- The VS Code task only fires on folder open (session start). There is no equivalent VS Code hook for session end — Copilot's chat instructions handle that via the MCP tool
- The `"reveal": "never"` task setting keeps the terminal panel hidden so the report fires silently
