---
name: skopos-report
description: Report current agent status to the Skopos dashboard. Use when you want to explicitly record what you are doing, planning, or blocked on.
---

Report your current status to Skopos using the report_status MCP tool.

Choose the most accurate status from: pending, thinking, planning, running, editing, testing, waiting, blocked, paused, handoff, succeeded, failed, cancelled.

Call the report_status tool with:
- agent_id: "claude-code-<hostname>" (use `hostname -s` output)
- agent_type: "claude-code"
- workspace: the current working directory
- status: the status that best describes what you are doing right now
- message: a short human-readable description (e.g. "investigating the auth bug", "waiting for user input", "blocked on missing API key")
- snippet: (optional) a one-line excerpt of relevant output or code
- git_branch: (optional) current git branch name

## Blackboard

Use the blackboard tools to share knowledge with other agents across sessions.

**Read the Knowledge Bundle at session start** (loads prior findings for the current branch):
Call `blackboard_read` with `branch` set to the current git branch name.

**Write an entry when you discover something worth sharing:**
Call `blackboard_write` with:

- scope: "session" (this session only), "branch" (shared on this branch), or "project" (all agents)
- branch_name: current branch (required when scope is "branch")
- entry_type: "finding", "decision", "bug", "debt", "warning", or "context"
- title: short description
- content: (optional) details
- code_ref: (optional) file and line, e.g. "auth/jwt.go:45"
- author_agent_id: "claude-code-HOSTNAME" (your machine's hostname)

Note: "bug" and "debt" entries are always visible to all agents regardless of branch.

## Plans

Use the plan tools to coordinate multi-step work across sessions.

**Create a plan at the start of a task:**
Call `plan_create` with:
- name: descriptive plan name
- branch_name: current branch (optional — omit for project-wide)
- author_agent_id: "claude-code-HOSTNAME"
- description: optional overview

**Add work items:**
Call `plan_add_item` with `plan_id`, `title`, and optional `description`.

**Update item progress:**
Call `plan_update_item` with `plan_id`, `item_id`, and:
- status: "pending", "in_progress", "done", or "blocked"
- claimed_by_agent_id: your agent ID (optional — pass empty string to release)

**Read a plan with all items:**
Call `plan_read` with `id` (the plan ID).
