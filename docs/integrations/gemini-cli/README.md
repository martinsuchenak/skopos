# Gemini CLI → Skopos Integration

## Prerequisites

- Skopos server running: `skopos serve` (HTTP on :8080, MCP at /mcp)
- `skopos` binary in PATH
- Gemini CLI installed (`gem install gemini-cli` or via your package manager)

> **Quick install:** `skopos install --agent gemini-cli [--url ...] [--api-key "$SKOPOS_API_KEY"]` does this for you — it merges the MCP config into `~/.gemini/settings.json` (idempotent, backs up existing config). Add `--scope project` to write into `.gemini/`. The manual steps below are the fallback.

## Step 1: Apply MCP config

Add the `skopos` entry from `settings-snippet.json` into the `mcpServers` section of `~/.gemini/settings.json`:

```json
"skopos": {
  "url": "http://localhost:8080/mcp",
  "type": "http",
  "trust": true,
  "headers": {
    "Authorization": "Bearer ${SKOPOS_API_KEY}"
  }
}
```

Do not replace the whole file — add only the `skopos` key to your existing `mcpServers` object.

> **Auth:** The `Authorization` header is only required when `auth.api_key` is set on the server; Gemini CLI expands `${SKOPOS_API_KEY}` from your environment. If your version doesn't expand it, paste the literal key.

## Step 2: Wire lifecycle hooks via a wrapper script

Gemini CLI does not have a built-in lifecycle hook system. Use a wrapper script that fires Skopos reports before and after each session:

Create `~/bin/gemini-skopos` (or any name you prefer):

```bash
#!/usr/bin/env bash
# Replace this with the absolute path to hooks.sh in your skopos repo:
HOOKS="/absolute/path/to/skopos/docs/integrations/gemini-cli/hooks.sh"

bash "$HOOKS" start
gemini "$@"
EXIT_CODE=$?
if [ "$EXIT_CODE" -eq 0 ]; then
  bash "$HOOKS" stop
else
  SKOPOS_SERVER_URL="${SKOPOS_SERVER_URL:-http://localhost:8080}" \
    skopos report \
      --agent-id "gemini-$(hostname -s)" \
      --agent-type gemini \
      --workspace "$PWD" \
      --status failed \
      --message "session ended with error" || true
fi
exit "$EXIT_CODE"
```

Make it executable and use `gemini-skopos` instead of `gemini` in your terminal:

```bash
chmod 755 ~/bin/gemini-skopos
```

Set environment variables (add to `~/.zshrc` or `~/.bashrc`):

```bash
export SKOPOS_API_KEY=your-key-here
export SKOPOS_SERVER_URL=http://localhost:8080
```

## Step 3: Verify

Run `gemini-skopos` in a project directory. Open `http://localhost:8080`. You should see a `gemini-<hostname>` agent with status `running`, then `succeeded` after exit.

## Blackboard

Once MCP is connected, Gemini CLI has access to `skopos__blackboard_write` and `skopos__blackboard_read` tools.

Read the Knowledge Bundle at session start to load prior findings:

```text
Call skopos__blackboard_read with branch set to the current git branch name.
```

Write an entry when you discover something worth sharing:

```text
Call skopos__blackboard_write with scope "branch", the current branch_name,
entry_type "finding"/"bug"/"decision"/etc., title, content, and author_agent_id.
```

`bug` and `debt` entries are always visible to all agents regardless of branch. Entries appear in the Skopos dashboard under the **Blackboard** tab at `http://localhost:8080`.

## Plans

Gemini CLI has access to `skopos__plan_create`, `skopos__plan_read`, `skopos__plan_add_item`, and `skopos__plan_update_item` tools.

Create a plan at the start of a multi-step task and track progress with items:

```text
Call skopos__plan_create with name, optional branch_name, and author_agent_id.
Call skopos__plan_add_item for each work item.
Call skopos__plan_update_item with status: "in_progress", "done", or "blocked" as you work.
```

Plans appear in the Skopos dashboard under the **Plans** tab at `http://localhost:8080`.

## Session IDs

Same resolution as other agents — see `shared/skopos-session.sh` for details. Set `$SKOPOS_SESSION_ID` to share a session across agents.
