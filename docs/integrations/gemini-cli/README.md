# Gemini CLI → Skopos Integration

## Prerequisites

- Skopos server running: `skopos serve` (REST on :8080, MCP on :9000)
- `skopos` binary in PATH
- Gemini CLI installed (`gem install gemini-cli` or via your package manager)

## Step 1: Apply MCP config

Add the `skopos` entry from `settings-snippet.json` into the `mcpServers` section of `~/.gemini/settings.json`:

```json
"skopos": {
  "url": "http://localhost:9000/mcp",
  "type": "http",
  "trust": true
}
```

Do not replace the whole file — add only the `skopos` key to your existing `mcpServers` object.

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

## Session IDs

Same resolution as other agents — see `shared/skopos-session.sh` for details. Set `$SKOPOS_SESSION_ID` to share a session across agents.
