# Kiro → Skopos Integration

## Prerequisites

- Skopos server running: `skopos serve` (HTTP on :8080, MCP at /mcp)
- `skopos` binary in PATH
- Kiro installed

> **Quick install:** `skopos install --agent kiro [--url ...] [--api-key "$SKOPOS_API_KEY"]` does this for you — it merges the MCP config and writes `.kiro/steering/skopos.md` (idempotent, backs up existing config). Add `--scope project` to target `.kiro/` in the current directory. The manual steps below are the fallback.

## Step 1: Apply MCP config (global)

Add the `skopos` entry from `mcp.json` into `~/.kiro/settings/mcp.json`:

```json
"skopos": {
  "url": "http://localhost:8080/mcp",
  "headers": {
    "Authorization": "Bearer ${SKOPOS_API_KEY}"
  }
}
```

Or for project-level, add to `.kiro/settings/mcp.json` in your workspace.

> **Auth:** The `Authorization` header is only required when `auth.api_key` is set on the server. If Kiro doesn't expand `${SKOPOS_API_KEY}` in your version, paste the literal key.

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

## Blackboard

Once MCP is connected, Kiro has access to `skopos__blackboard_write` and `skopos__blackboard_read` tools.

Add `"blackboard_read"` and `"blackboard_write"` to the `autoApprove` list in `mcp.json` so Kiro can call them without prompting:

```json
"skopos": {
  "url": "http://localhost:8080/mcp",
  "autoApprove": ["report_status", "blackboard_read", "blackboard_write", "plan_create", "plan_read", "plan_add_item", "plan_update_item"]
}
```

See the steering snippet for usage instructions. Entries appear in the Skopos dashboard under the **Blackboard** tab at `http://localhost:8080`.

## Plans

Once MCP is connected, Kiro has access to `skopos__plan_create`, `skopos__plan_read`, `skopos__plan_add_item`, and `skopos__plan_update_item` tools.

See the steering snippet for plan usage instructions. Plans appear in the Skopos dashboard under the **Plans** tab at `http://localhost:8080`.

## Notes

- The MCP tool name in Kiro will appear as `skopos__report_status` (server name + double underscore + tool name)
- Kiro may request approval before calling MCP tools — add `"autoApprove": ["report_status"]` to the `skopos` entry in `mcp.json` to auto-approve
