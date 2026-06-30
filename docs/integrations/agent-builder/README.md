# skopos → agent-builder artifacts

Canonical [agent-builder](https://github.com/martinsuchenak/agent-builder) source
that equips an AI agent with skopos skills, commands, and rules — compiled from
one source into the native formats of Claude Code, opencode, Codex, GitHub
Copilot, and Kiro.

This is the **companion** to `skopos install`:

- `skopos install` wires the **MCP server connection** (the `skopos` MCP entry + auth header) into an agent.
- This artifact set equips the agent with **skills/commands/rules that use skopos** (load context, report status, remember findings, always-on guidance).

Use both: connect, then equip.

## Artifacts

| Kind | ID | What it does |
|------|----|--------------|
| skill | `skopos` | How to use skopos memory/plans/status (native Agent Skills — all targets) |
| command | `skopos-context` | Load the branch's blackboard, active/blocked plans, in-flight sessions |
| command | `skopos-report` | Send a status checkpoint to the dashboard |
| command | `skopos-remember` | Write a finding/decision/bug to the blackboard |
| rule | `skopos` | Always-on session guidance (merges into CLAUDE.md / AGENTS.md / steering) |

Tool calls use the inline `@skopos` server binding, e.g.
`{{tool skopos_context@skopos branch="feat-auth"}}`, which agent-builder compiles
to each target's native invocation form.

## Prerequisites

- The skopos server running and its MCP server installed into your agent:
  ```bash
  skopos install --agent claude-code   # (or your agent) — wires the MCP connection
  ```
- agent-builder: see https://github.com/martinsuchenak/agent-builder

## Compile & install

From the skopos repo root:

```bash
# 1. Validate the canonical source
agent-builder validate docs/integrations/agent-builder/ab-src

# 2. Compile to every target (writes ./build/<target>/)
agent-builder compile docs/integrations/agent-builder/ab-src

# 3. Install into each target's user config dir
agent-builder install
```

One target only:

```bash
agent-builder compile docs/integrations/agent-builder/ab-src --target claude
agent-builder install --target claude
```

Existing managed-region files (`CLAUDE.md`, `AGENTS.md`, …) are merged, so
hand-written content outside the skopos region is preserved. Run `agent-builder help`
for the full CLI.

## Layout

```
ab-src/artifacts/
  skills/skopos/SKILL.md        # Agent Skills standard
  commands/skopos-context.md
  commands/skopos-report.md
  commands/skopos-remember.md
  rules/skopos.md
```
