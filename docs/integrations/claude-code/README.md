# Claude Code → Skopos Integration

Two layers: MCP (rich voluntary reporting) + hooks (automatic lifecycle reporting).

## Prerequisites

- Skopos server running: `skopos serve` (HTTP on :8080, MCP at /mcp)
- `skopos` binary in PATH: `sudo ln -sf $(pwd)/bin/skopos /usr/local/bin/skopos`
- `jq` installed: `brew install jq`

> **Quick install:** `skopos install --agent claude-code [--url ...] [--api-key "$SKOPOS_API_KEY"]` does Step 1 for you — it merges the MCP config into `~/.claude/settings.json` and installs the skill (idempotent, backs up existing config). Add `--scope project` to write into `.claude/` instead. The manual steps below are the fallback.

## Step 1: Apply MCP + hooks config

Find the absolute path to the hooks script:

```bash
echo "$(pwd)/docs/integrations/claude-code/hooks.sh"
```

Open `~/.claude/settings.json` (create it if it doesn't exist) and merge in the contents of `settings-snippet.json`, replacing `SKOPOS_HOOKS_PATH` with the path above.

If you already have `mcpServers` or `hooks` sections, add the `skopos` entries to the existing objects — do not replace the whole file.

> **Auth:** The snippet's `skopos` MCP server sends `Authorization: Bearer ${SKOPOS_API_KEY}`. This header is only required when `auth.api_key` is set on the server; Claude Code expands the env var automatically.

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
