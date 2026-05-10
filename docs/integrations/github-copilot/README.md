# GitHub Copilot → Skopos Integration

Two layers: MCP (rich voluntary reporting via Copilot Chat) + VS Code workspace task (automatic session start on folder open).

## Prerequisites

- Skopos server running: `skopos serve` (REST on :8080, MCP on :9000)
- `skopos` binary in PATH
- GitHub Copilot extension installed in VS Code

## Step 1: Apply MCP config

Add the `skopos` entry from `mcp-snippet.json` into VS Code's global MCP config:

**File:** `~/Library/Application Support/Code/User/mcp.json`

```json
"skopos": {
  "type": "http",
  "url": "http://localhost:9000/mcp"
}
```

Add it under the existing `"servers"` object. Create the file if it doesn't exist:

```json
{
  "servers": {
    "skopos": {
      "type": "http",
      "url": "http://localhost:9000/mcp"
    }
  }
}
```

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

## Notes

- The MCP tool name in Copilot Chat appears as `skopos__report_status` (server name + double underscore + tool name)
- The VS Code task only fires on folder open (session start). There is no equivalent VS Code hook for session end — Copilot's chat instructions handle that via the MCP tool
- The `"reveal": "never"` task setting keeps the terminal panel hidden so the report fires silently
