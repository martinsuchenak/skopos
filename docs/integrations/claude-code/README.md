# Claude Code → Skopos Integration

Two layers: MCP (rich voluntary reporting) + hooks (automatic lifecycle reporting).

## Prerequisites

- Skopos server running: `skopos serve` (HTTP on :8080, MCP at /mcp)
- `skopos` binary in PATH: `sudo ln -sf $(pwd)/bin/skopos /usr/local/bin/skopos`
- `jq` installed: `brew install jq`

> **Quick install:** `skopos install --agent claude-code [--url ...] [--api-key "$SKOPOS_API_KEY"]` does Steps 1–2 for you: it merges the MCP entry into `~/.claude.json` (user scope; `--scope project` writes `.mcp.json` instead — Claude Code does not read `mcpServers` from `settings.json`), installs the hook suite into `~/.claude/hooks/` and registers it in `~/.claude/settings.json`, writes the `/skopos` (code exploration) and `/skopos-report` commands, and manages the behavioral block in `~/.claude/CLAUDE.md` between `<!-- skopos:begin -->` / `<!-- skopos:end -->` markers. Idempotent; backs up existing config. The manual steps below are the fallback.

## Step 1: Apply MCP + hooks config

MCP servers go into `~/.claude.json` (top-level `mcpServers`, user scope) or `.mcp.json` in the repo root (project scope) — **not** `settings.json`, which only carries hooks and hook-adjacent settings:

```json
{
  "mcpServers": {
    "skopos": {
      "type": "http",
      "url": "http://localhost:8080/mcp",
      "headers": { "Authorization": "Bearer ${SKOPOS_API_KEY}" }
    }
  }
}
```

> The `type` field is required: an entry with a `url` but no `type` is treated as stdio and skipped.

For hooks, find the absolute path to the hooks script:

```bash
echo "$(pwd)/docs/integrations/claude-code/hooks.sh"
```

Open `~/.claude/settings.json` (create it if it doesn't exist) and merge in the `hooks` section of `settings-snippet.json`, replacing `SKOPOS_HOOKS_PATH` with the path above. If you already have `hooks`, add the `skopos` entries to the existing object — do not replace the whole file.

> **Auth:** The `Authorization` header is only required when `auth.api_key` is set on the server; in `.mcp.json` Claude Code expands `${SKOPOS_API_KEY}` from your environment automatically.

Set your API key in the environment (add to `~/.zshrc` or `~/.bashrc`):

```bash
export SKOPOS_API_KEY=your-key-here
export SKOPOS_SERVER_URL=http://localhost:8080
```

## Step 2: Install the slash command (optional, for manual reporting)

Copy `skopos-skill.md` to the Claude Code commands directory:

```bash
mkdir -p ~/.claude/commands
cp docs/integrations/claude-code/skopos-skill.md ~/.claude/commands/skopos-report.md
```

Then use `/skopos-report` in any Claude Code session to report rich status.

## Step 3: Verify

Start a Claude Code session in any directory. Open the Skopos dashboard at `http://localhost:8080`. Use a tool (e.g. ask Claude to run `ls`). You should see a new session appear with status `running`.

## Blackboard

Once MCP is connected, Claude Code automatically has access to two blackboard tools:

- **`blackboard_write`** — record a finding, decision, bug, debt, warning, or context note
- **`blackboard_read`** — fetch the Knowledge Bundle for the current branch (structured entries + formatted markdown)

Typical workflow:

```text
# At session start — load prior context
blackboard_read(branch: "feat-auth")

# During work — record discoveries
blackboard_write(
  scope: "branch", branch_name: "feat-auth",
  entry_type: "finding", title: "JWT expiry not checked on refresh",
  content: "Refresh tokens bypass expiry validation entirely.",
  code_ref: "auth/jwt.go:45", author_agent_id: "claude-code-macbook"
)

# Critical issues float to all branches automatically (use scope: "project" or entry_type: "bug"/"debt")
```

Entry types: `finding`, `decision`, `bug`, `debt`, `warning`, `context`
Scopes: `session` (this session only), `branch` (shared per branch), `project` (all agents)

`bug` and `debt` entries are always visible across all branches regardless of scope.

Entries are visible in the Skopos dashboard under the **Blackboard** tab at `http://localhost:8080`.

## Plans

Once MCP is connected, Claude Code has access to plan tools for coordinating work across sessions:

- **`plan_create`** — create a named plan, optionally scoped to a branch
- **`plan_read`** — fetch a plan with all its items
- **`plan_add_item`** — add a work item to a plan
- **`plan_update_item`** — update item status or claim it

Typical workflow:

```text
# At session start — create a plan for this task
plan_create(name: "Auth refactor", branch_name: "feat-auth", author_agent_id: "claude-code-macbook")

# During work — add items and update status
plan_add_item(plan_id: "...", title: "Audit refresh token logic")
plan_update_item(plan_id: "...", item_id: "...", status: "in_progress", claimed_by_agent_id: "claude-code-macbook")

# Mark items done as you complete them
plan_update_item(plan_id: "...", item_id: "...", status: "done")
```

Item statuses: `pending`, `in_progress`, `done`, `blocked`

Plans are visible in the Skopos dashboard under the **Plans** tab at `http://localhost:8080`.

## Session IDs

Sessions are resolved in this order:
1. `$SKOPOS_SESSION_ID` env var
2. `.skopos-session` file in the workspace root
3. Auto-generated hash (stable per workspace per day)

To share a session across agents, set `export SKOPOS_SESSION_ID=my-session` in your shell before starting any agents.
