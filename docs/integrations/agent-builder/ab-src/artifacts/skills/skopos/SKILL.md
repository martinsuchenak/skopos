---
name: skopos
description: Use the skopos shared memory, plans, status, and code index service. At task start call skopos_context to load prior findings and active plans; explore code via the code_search/code_callers/code_impact tools before grep; write findings/decisions/bugs to the blackboard; track multi-step work in plans; checkpoint progress with report_status.
---

Skopos is a shared memory and coordination service for AI agents, spanning sessions and git branches. Four capabilities:

## Code intelligence
A central code index answering structure questions faster than grep.
- **Find symbols:** `code_search` (names/signatures, prefix match), `code_symbol` (exact definition with file:line)
- **Structure:** `code_outline` (a file's definitions in source order), `code_callers` / `code_callees` (the call graph around a symbol), `code_call_tree`
- **Risk:** `code_impact` (what transitively breaks if a symbol changes), `code_branch_diff` (index-level diff against the default branch), `code_dead` / `code_cycles`
- All take the workspace's `workspace_id`; `branch` is optional — an unindexed branch answers from the default branch with a fallback note in the response.
- Check the surface once per session with `skopos mode` (shell): `remote <url>` → the MCP tools above; `local` → the CLI equivalents (`skopos search`, `symbol`, `outline`, `who-calls`, `call-tree`, `impact`, `branch-diff`; all accept `--json`).
Use skopos first for "how does X work", "who calls Y", or blast-radius questions. Grep is right for string literals, config values, env vars, non-symbol text, and exhaustive "find ALL occurrences" listings.

## Memory (blackboard)
Durable knowledge entries — your notebook across sessions.
- **Recall:** call `blackboard_read` with the current git `branch` to load prior findings/decisions/bugs.
- **Record:** call `blackboard_write` with `scope` (`project` = all agents/branches, `branch`, `session`), `entry_type` (`finding`, `decision`, `bug`, `debt`, `warning`, `context`), a short `title`, optional `content` and `code_ref`, and a stable `author_agent_id`.
- `bug` and `debt` entries float — always returned regardless of branch. Use them for issues every agent must see.

## Plans (todos)
Shared to-do lists with dependencies.
- `plan_create` (name, optional `branch_name`, `author_agent_id`) then `plan_add_item` for each work item.
- Update progress with `plan_update_item` (`status`: `pending`, `in_progress`, `done`, `blocked`).
- Adding a dependency auto-blocks the dependent item; finishing a dependency auto-unblocks it; finishing every item auto-completes the plan.

## Status
- Checkpoint with `report_status` (`status`, `progress`, `message`). Values: `thinking`, `planning`, `running`, `editing`, `testing`, `waiting`, `blocked`, `paused`, `handoff`, `succeeded`, `failed`, `cancelled`.
- Never report `stuck` or `orphaned` — those are set by the server's health checker.

## Routine
1. At task start: call `skopos_context` (pass `branch`) to load the blackboard, active/blocked plan items, and in-flight sessions.
2. Explore code through the index before grepping; record discoveries to the blackboard and track multi-step work in plans.
3. Checkpoint with `report_status`.

Keep entries concise, prefer the narrowest scope, and reuse one stable `author_agent_id` (e.g. `<tool>-<hostname>`).
