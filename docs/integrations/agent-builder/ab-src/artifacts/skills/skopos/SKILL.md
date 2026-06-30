---
name: skopos
description: Use the skopos shared memory, plans, and status service. At task start call skopos_context to load prior findings and active plans; write findings/decisions/bugs to the blackboard; track multi-step work in plans; checkpoint progress with report_status.
---

Skopos is a shared memory and coordination service for AI agents, spanning sessions and git branches. Three capabilities:

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
2. Work; record discoveries to the blackboard and track multi-step work in plans.
3. Checkpoint with `report_status`.

Keep entries concise, prefer the narrowest scope, and reuse one stable `author_agent_id` (e.g. `<tool>-<hostname>`).
