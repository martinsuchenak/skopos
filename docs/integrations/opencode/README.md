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

## Session IDs

Set `$SKOPOS_SESSION_ID` in your shell to share a session across agents working in the same workspace.
