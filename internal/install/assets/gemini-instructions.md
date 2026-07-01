## Skopos Integration

You have access to Skopos MCP tools for shared memory, plans, and status reporting.

**At the start of every task**, call `skopos_context` with `workspace_id` and `branch` to load prior findings, active plans, and in-flight sessions.

**Recording knowledge** — call `blackboard_write` with:
- `scope`: "branch" (shared on this branch) or "project" (all agents)
- `branch_name`: current branch (required when scope is "branch")
- `workspace_id`: your workspace ID
- `entry_type`: "finding", "decision", "bug", "debt", "warning", or "context"
- `title`, `content`, `author_agent_id`

**Recalling knowledge** — call `blackboard_read` with `workspace_id` and `branch`.

**Planning work** — call `plan_create`, then `plan_add_item` for each task. Update item status with `plan_update_item` as you progress. Check blocked items with `plan_read` (pass `item_id` for a single-item check).

**Status reporting** — call `report_status` with your `agent_id`, `agent_type` ("gemini"), `workspace_id`, and `status` (one of: pending, thinking, planning, running, editing, testing, waiting, blocked, paused, handoff, succeeded, failed, cancelled). Never report "stuck" or "orphaned".
