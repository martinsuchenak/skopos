# OpenCode → Skopos Integration

## Prerequisites

- Skopos server running: `skopos serve` (HTTP on :8080, MCP at /mcp)
- `skopos` binary in PATH
- OpenCode installed

> **Quick install:** `skopos install --agent opencode [--url ...] [--api-key "$SKOPOS_API_KEY"]` does this for you — it merges the MCP config into `~/.config/opencode/opencode.json` (idempotent, backs up existing config). Add `--scope project` to write into `./opencode.json`. The manual steps below are the fallback.

## Step 1: Apply MCP config

Add the `skopos` entry from `config-snippet.json` into the `mcp` section of `~/.config/opencode/opencode.json`:

```json
"mcp": {
  "skopos": {
    "type": "remote",
    "url": "http://localhost:8080/mcp",
    "headers": {
      "Authorization": "Bearer ${SKOPOS_API_KEY}"
    }
  }
}
```

> **Auth:** The `Authorization` header is only required when `auth.api_key` is set on the server; OpenCode expands `${SKOPOS_API_KEY}` from your environment.

> **Note:** OpenCode MCP server format may vary by version. If `"type": "remote"` is not accepted, try omitting it (just `"url": "http://localhost:8080/mcp"`). Verify against your version's schema at `https://opencode.ai/config.json`.

## Step 2: Wire lifecycle hooks via AGENTS.md

OpenCode reads `AGENTS.md` files for behavioral instructions. Add the following to your project's `AGENTS.md`:

```markdown
## Skopos Status Reporting

Report your status to the Skopos dashboard via the `skopos__report_status` MCP tool.

When starting a session: call `skopos__report_status` with `agent_type: "opencode"`, `status: "running"`, `message: "session started"`.

When completing successfully: call with `status: "succeeded"`, `message: "session complete"`.

When completing with errors: call with `status: "failed"`, `message: "session ended with error"`.

During work, optionally report granular status: `thinking`, `planning`, `editing`, `testing`, `blocked`.
```

Set environment variables (add to `~/.zshrc` or `~/.bashrc`):

```bash
export SKOPOS_API_KEY=your-key-here
export SKOPOS_SERVER_URL=http://localhost:8080
```

## Step 3: Verify

Start an OpenCode session in your project. Open `http://localhost:8080`. You should see an `opencode-<hostname>` agent appear after the agent calls the `skopos__report_status` tool.

## Blackboard

Once MCP is connected, OpenCode has access to `skopos__blackboard_write` and `skopos__blackboard_read` tools.

Add to your project's `AGENTS.md`:

```markdown
## Skopos Blackboard

At the start of a session, call `skopos__blackboard_read` with `branch` set to
the current git branch to load prior findings from other agents.

When you discover something worth recording, call `skopos__blackboard_write` with
scope "branch" or "project", the appropriate entry_type (finding, decision, bug,
debt, warning, context), a short title, and optional content and code_ref.
Use entry_type "bug" or "debt" for critical issues — these are always visible
to all agents regardless of branch.
```

Entries appear in the Skopos dashboard under the **Blackboard** tab at `http://localhost:8080`.

## Plans

OpenCode has access to `skopos__plan_create`, `skopos__plan_read`, `skopos__plan_add_item`, and `skopos__plan_update_item` tools.

Add to your project's `AGENTS.md`:

```markdown
## Skopos Plans

At the start of a multi-step task, call `skopos__plan_create` with a name, optional
branch_name, and your author_agent_id. Then call `skopos__plan_add_item` for each
work item. Update item status with `skopos__plan_update_item` as you work.
Item statuses: pending, in_progress, done, blocked.
```

Plans appear in the Skopos dashboard under the **Plans** tab at `http://localhost:8080`.

## Session IDs

Set `$SKOPOS_SESSION_ID` in your shell to share a session across agents working in the same workspace.
