# Kiro → Skopos Integration

## Prerequisites

- Skopos server running: `skopos serve` (REST on :8080, MCP on :9000)
- `skopos` binary in PATH
- Kiro installed

## Step 1: Apply MCP config (global)

Add the `skopos` entry from `mcp.json` into `~/.kiro/settings/mcp.json`:

```json
"skopos": {
  "url": "http://localhost:9000/mcp"
}
```

Or for project-level, add to `.kiro/settings/mcp.json` in your workspace.

## Step 2: Wire lifecycle hooks via steering document

Copy the steering snippet to your project's Kiro steering directory:

```bash
mkdir -p .kiro/steering
cp docs/integrations/kiro/steering-snippet.md .kiro/steering/skopos.md
```

Kiro reads all `.md` files in `.kiro/steering/` as behavioral instructions. This will instruct Kiro to call the `skopos__report_status` MCP tool (which is the Skopos MCP tool, prefixed with the server name) at session start, completion, and failure.

## Step 3: Verify

Start a Kiro session in the project directory. Open `http://localhost:8080`. You should see a `kiro-<hostname>` agent appear.

## Session IDs

Kiro does not auto-set `session_id` when calling the MCP tool. Either:
- Set `$SKOPOS_SESSION_ID` in your shell before starting Kiro, or
- Leave it empty — the server will create a new session per run, grouped by agent ID

## Notes

- The MCP tool name in Kiro will appear as `skopos__report_status` (server name + double underscore + tool name)
- Kiro may request approval before calling MCP tools — add `"autoApprove": ["report_status"]` to the `skopos` entry in `mcp.json` to auto-approve
