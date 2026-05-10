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
