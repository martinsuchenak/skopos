---
id: skopos
kind: rule
description: Always-on skopos session guidance — code exploration via the index before grep, load context at start, record to the blackboard during work, track work in plans, checkpoint status.
targets: [claude, opencode, codex, copilot, kiro]
---
## Mandatory: Code exploration via skopos

In any project with a skopos code index, ALWAYS use skopos as the FIRST step for code exploration, architecture understanding, or symbol lookup. This applies to:
- Answering questions about how something works
- Finding symbols, callers, dependencies, file outlines
- Understanding code before making changes
- Assessing the blast radius of a change

**Do NOT** use grep/find for code structure or delegate code search to sub-agents until skopos has been tried first.

Check once per session which skopos surface you have (shell: `skopos mode`):
- `remote <url>` — skopos MCP tools: `code_search` (names/signatures), `code_symbol` (exact definitions with file:line), `code_outline` (a file's definitions), `code_callers` / `code_callees` (call graph), `code_impact` (what transitively breaks), `code_branch_diff` (index diff against the default branch)
- `local` — no MCP tools; same queries via the CLI: `skopos search`, `skopos symbol`, `skopos outline`, `skopos who-calls`, `skopos call-tree`, `skopos impact`, `skopos branch-diff` (all accept `--json`)

Only fall back to grep for: string literals, config values, env vars, non-symbol text, or exhaustive "find ALL occurrences" listings.

## Shared memory — mandatory behavior

When a skopos server is reachable (remote mode), you MUST proactively save knowledge to the blackboard **during** the session, immediately when something is decided or discovered — do not wait to be asked or for the session to end:

- Architectural decisions (the why) → `blackboard_write` entry_type `decision`
- Bugs found or fixed → `bug`; known debt → `debt` (these float — always visible regardless of branch)
- Conventions and patterns → `context` or `finding`
- Scope: `branch` by default (shared with everyone on this branch), `project` for repo-wide facts
- Obsolete entries → `blackboard_delete` (entry IDs come from `blackboard_read`)

Be selective — only facts useful in a future session. Skip task details and temporary state.

## Session cadence (remote mode)

- Start of task: `skopos_context` — prior knowledge, active plans, in-flight sessions
- State changes: `report_status` (never "stuck"/"orphaned" — server-set)
- Multi-step work: `plan_create` / `plan_add_item` / `plan_update_item`; archive with `plan_archive` when done or abandoned
