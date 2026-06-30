---
id: skopos
kind: rule
description: Always-on skopos session guidance — load context at start, record to the blackboard, track work in plans, checkpoint status.
targets: [claude, opencode, codex, copilot, kiro]
---
## Skopos

You have a skopos MCP server with shared memory, plans, and status. Use it throughout your work.

- **At the start of a task:** call `skopos_context` (with the current git branch) to load prior findings, active/blocked plan items, and in-flight sessions. Don't redo known work.
- **When you discover something worth keeping:** call `blackboard_write`. Use `entry_type` `bug` or `debt` for issues every agent must see (they float across branches); `finding`/`decision`/`warning`/`context` otherwise. Default `scope` is `branch`; use `project` for repo-wide. Always set a stable `author_agent_id`.
- **For multi-step work:** create a plan (`plan_create`), add items (`plan_add_item`), and update their status (`plan_update_item`) as you go. Dependencies auto-block/unblock.
- **Checkpoint progress:** call `report_status` when you start, at milestones, and on success (`succeeded`) or failure (`failed`). Never report `stuck` or `orphaned` — those are set by the server.

Keep entries concise and reuse one `author_agent_id` for the whole session.
