# OpenCode → Skopos Integration

## Prerequisites

- Skopos server running: `skopos serve` (REST on :8080, MCP on :9000)
- `skopos` binary in PATH
- OpenCode installed

## Step 1: Apply MCP config

Add the `skopos` entry from `config-snippet.json` into the `mcp` section of `~/.config/opencode/opencode.json`:

```json
"mcp": {
  "skopos": {
    "type": "remote",
    "url": "http://localhost:9000/mcp"
  }
}
```

> **Note:** OpenCode MCP server format may vary by version. If `"type": "remote"` is not accepted, try omitting it (just `"url": "http://localhost:9000/mcp"`). Verify against your version's schema at `https://opencode.ai/config.json`.

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

## Session IDs

Set `$SKOPOS_SESSION_ID` in your shell to share a session across agents working in the same workspace.
