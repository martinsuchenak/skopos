# ZCode → Skopos Integration

ZCode connects to skopos over MCP and picks up the same behavioral assets as the other agents.

## Prerequisites

- Skopos server running: `skopos serve` (HTTP on :8080, MCP at `/mcp`)
- `skopos` binary in PATH, `jq` installed (hook scripts use both)

> **Quick install:** `skopos install --agent zcode [--url ...] [--api-key "$SKOPOS_API_KEY"]` does all three steps for you — it merges the MCP entry into `~/.zcode/cli/config.json` (`mcp.servers.skopos`), writes the `/skopos` and `/skopos-report` commands to `~/.zcode/commands/`, manages the behavioral block in `~/.zcode/AGENTS.md` between skopos markers, installs the hook suite, and sets `hooks.enabled: true` (ZCode runs configuration-file hooks only when that flag is set). Idempotent; backs up existing config. Use `--scope project` to write `.zcode/` in the current repo instead (instructions then go to the repo's `AGENTS.md`). The manual steps below are the fallback.

> **Scoped keys:** prefer minting a per-machine key with `skopos key create --workspace <id>` over sharing the root key — see [concepts/api-keys.md](../concepts/api-keys.md).


## Step 1: MCP config

Add to `mcp.servers` in `~/.zcode/cli/config.json` (or `.zcode/config.json` in a repo for team scope):

```json
"skopos": {
  "type": "http",
  "url": "https://skopos.example.com/mcp",
  "headers": { "Authorization": "Bearer $SKOPOS_API_KEY" }
}
```

Servers from both scopes auto-connect at session start. MCP servers bind when a session begins — restart the session after adding one.

## Step 2: Hooks

The hook suite (session briefing, prompt-time code pre-fetch, search nudges, memory reminders) lives in `~/.zcode/hooks/skopos-*.sh` and registers under `hooks.events` with the same events as Claude Code — ZCode supports `SessionStart`, `UserPromptSubmit`, `PreToolUse`, `PostToolUse`, and `Stop`, with **case-sensitive regex matchers** over tool names:

```json
"hooks": {
  "enabled": true,
  "events": {
    "SessionStart": [ { "hooks": [ { "type": "command", "command": "~/.zcode/hooks/skopos-session.sh" } ] } ],
    "PreToolUse": [
      { "matcher": "Grep",  "hooks": [ { "type": "command", "command": "~/.zcode/hooks/skopos-pre-tool.sh" } ] },
      { "matcher": "Agent", "hooks": [ { "type": "command", "command": "~/.zcode/hooks/skopos-pre-tool.sh" } ] },
      { "matcher": "Bash",  "hooks": [ { "type": "command", "command": "~/.zcode/hooks/skopos-pre-tool.sh" } ] }
    ]
  }
}
```

Note `hooks.enabled: true` — without it, configuration-file hooks never fire. The hooks are mode-aware (`skopos mode`): with a server configured they steer agents to the MCP tools; in local-only mode they steer to the CLI. Opt out entirely with `skopos install --agent zcode --no-hooks`.

## Step 3: Slash commands and instructions

Commands are markdown files in `~/.zcode/commands/` — copy `skopos-skill.md` as `skopos-report.md` and any exploration command as `skopos.md`. The behavioral block (exploration-first rules, memory mandate) goes to `~/.zcode/AGENTS.md`; a repo's own `AGENTS.md` loads after the user file and can narrow it.

## Session IDs

Same resolution as other agents — `$SKOPOS_SESSION_ID` shares a session across agents.
