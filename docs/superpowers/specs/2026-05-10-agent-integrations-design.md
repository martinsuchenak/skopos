# Agent Integrations Design

**Date:** 2026-05-10
**Status:** Approved

## Summary

Generate integration files for five AI agents (Claude Code, Gemini CLI, Codex, Kiro, OpenCode) so they all report status into the Skopos dashboard. Each agent gets two integration layers: automatic lifecycle hooks and an MCP tool config. A shared session-ID helper groups agents working in the same workspace into one dashboard session.

## Goals

- Populate the Skopos dashboard with real traffic from day one, with zero changes to the Skopos server.
- Cover all five agents: Claude Code, Gemini CLI, Codex, Kiro, OpenCode.
- Fail silently — a downed Skopos server must never interrupt agent work.

## Architecture

### Layer 1: MCP (agent-driven, high-fidelity)

Each agent is configured to connect to the Skopos MCP server at `http://localhost:9000/mcp`. The `report_status` tool is already implemented and live. The agent's AI calls it voluntarily with rich status (`thinking`, `planning`, `blocked`, etc.) and a human-readable message or snippet.

For remote agents (not on localhost), the REST endpoint `POST /api/reports` with `X-API-Key` header is the alternative — used via `curl` in hook scripts when the skopos binary is unavailable.

### Layer 2: Hooks (automatic, lifecycle baseline)

Each agent has hook scripts that fire `skopos report` CLI on lifecycle events. Hook calls always include `|| true` so failures are silent.

### Session ID

A shared shell helper (`skopos-session.sh`) resolves the session ID in this order:

1. `$SKOPOS_SESSION_ID` env var (explicit override)
2. `.skopos-session` file in the workspace root (persisted across terminal sessions)
3. Stable hash of workspace path + current date (auto-generated fallback)

Agents working in the same directory share a session automatically without coordination.

## Hook Event Mapping

| Event | Status | Message |
|---|---|---|
| Session/conversation start | `running` | `"session started"` |
| PreToolUse | `running` | `"using <tool_name>"` |
| PostToolUse (success) | `running` | `"<tool_name> complete"` |
| Session stop (success) | `succeeded` | `"session complete"` |
| Session stop (error) | `failed` | `"session ended with error"` |

All hook reports include: `--agent-id` (stable, e.g. `claude-code-<hostname>`), `--agent-type`, `--workspace` (cwd), `--session-id` (from helper).

MCP reports are agent-authored: full payload including `progress`, `step_current`, `step_total`, `message`, `snippet`.

## File Structure

```
docs/integrations/
  shared/
    skopos-session.sh          # session ID helper, sourced by all hooks
    setup.sh                   # interactive setup walkthrough
    test-smoke.sh              # end-to-end smoke test against a live server
  claude-code/
    README.md
    settings-snippet.json      # MCP server + Stop/PreToolUse/PostToolUse hooks
    skopos-skill.md            # skill file for manual /skopos-report command
  gemini-cli/
    README.md
    settings-snippet.json      # MCP server entry for ~/.gemini/settings.json
    hooks.sh                   # hook script for Gemini lifecycle events
  codex/
    README.md
    config-snippet.yaml        # MCP server entry for ~/.codex/config.yaml
    AGENTS-snippet.md          # hook instructions block for AGENTS.md
  kiro/
    README.md
    mcp.json                   # MCP server config for .kiro/settings/mcp.json
    hooks-snippet.json         # lifecycle hook entries for Kiro
  opencode/
    README.md
    config-snippet.json        # MCP + hook config for ~/.config/opencode/config.json
```

## Per-Agent Integration Details

### Claude Code

- **MCP config:** Add `"skopos"` entry under `mcpServers` in `~/.claude/settings.json` (or project `.claude/settings.json`) with `"type": "http"` and `"url": "http://localhost:9000/mcp"`.
- **Hooks:** `Stop`, `PreToolUse`, `PostToolUse` hooks in `settings.json` calling `skopos report` via shell command.
- **Skill:** `skopos-skill.md` dropped into `~/.claude/plugins/skills/` gives a `/skopos-report` slash command for explicit rich-status reporting.

### Gemini CLI

- **MCP config:** `mcpServers` entry in `~/.gemini/settings.json` (same structure as Claude Code).
- **Hooks:** Shell script wired to Gemini's hook mechanism (session start/stop events).

### Codex (OpenAI Codex CLI)

- **MCP config:** `mcpServers` entry in `~/.codex/config.yaml`.
- **Hooks:** Hook instructions appended to project `AGENTS.md` — Codex reads AGENTS.md for shell hook configuration.

### Kiro

- **MCP config:** `mcp.json` placed at `.kiro/settings/mcp.json` in the workspace.
- **Hooks:** Lifecycle hook entries in Kiro's hook configuration system.

### OpenCode

- **MCP config + hooks:** Config block in `~/.config/opencode/config.json`.

> **Note:** Claude Code's config format is known precisely. For Gemini CLI, Codex, Kiro, and OpenCode the exact schema will be verified against current documentation during implementation — snippets may need minor format adjustments.

## Error Handling

- All hook scripts use `|| true` — Skopos server down = silent skip, agent unaffected.
- MCP calls that fail are handled by the agent's existing error handling (tool errors don't crash the agent).
- Hooks do not retry.

## Testing

- **`test-smoke.sh`:** Starts a local Skopos server, fires `skopos report` via CLI, fires a `curl` REST call, optionally exercises the MCP tool. Checks the session appears in `GET /api/sessions`. Runs in ~5 seconds, no external dependencies.
- **Hook scripts:** Validated by running directly against a live server.
- **Skill file:** Validated manually — drop into Claude Code, invoke `/skopos-report`, confirm dashboard updates.

## Out of Scope

- Automatic installation (users apply files manually).
- Agent-specific authentication beyond the single `SKOPOS_API_KEY`.
- Windows support (hooks are shell scripts; bat/ps1 equivalents are a future addition).
