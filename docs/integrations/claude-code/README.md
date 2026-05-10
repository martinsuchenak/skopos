# Claude Code → Skopos Integration

Two layers: MCP (rich voluntary reporting) + hooks (automatic lifecycle reporting).

## Prerequisites

- Skopos server running: `skopos serve` (REST on :8080, MCP on :9000)
- `skopos` binary in PATH: `sudo ln -sf $(pwd)/bin/skopos /usr/local/bin/skopos`
- `jq` installed: `brew install jq`

## Step 1: Apply MCP + hooks config

Find the absolute path to the hooks script:

```bash
echo "$(pwd)/docs/integrations/claude-code/hooks.sh"
```

Open `~/.claude/settings.json` (create it if it doesn't exist) and merge in the contents of `settings-snippet.json`, replacing `SKOPOS_HOOKS_PATH` with the path above.

If you already have `mcpServers` or `hooks` sections, add the `skopos` entries to the existing objects — do not replace the whole file.

Set your API key in the environment (add to `~/.zshrc` or `~/.bashrc`):

```bash
export SKOPOS_API_KEY=your-key-here
export SKOPOS_SERVER_URL=http://localhost:8080
```

## Step 2: Install the skill (optional, for manual reporting)

Copy `skopos-skill.md` to your skills directory:

```bash
cp docs/integrations/claude-code/skopos-skill.md ~/.claude/plugins/skills/
```

Then use `/skopos-report` in any Claude Code session to report rich status.

## Step 3: Verify

Start a Claude Code session in any directory. Open the Skopos dashboard at `http://localhost:8080`. Use a tool (e.g. ask Claude to run `ls`). You should see a new session appear with status `running`.

## Session IDs

Sessions are resolved in this order:
1. `$SKOPOS_SESSION_ID` env var
2. `.skopos-session` file in the workspace root
3. Auto-generated hash (stable per workspace per day)

To share a session across agents, set `export SKOPOS_SESSION_ID=my-session` in your shell before starting any agents.
