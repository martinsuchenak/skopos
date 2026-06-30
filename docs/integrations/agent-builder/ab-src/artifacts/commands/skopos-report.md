---
id: skopos-report
kind: command
description: Report your current status to the skopos dashboard.
targets: [claude, opencode, codex, copilot, kiro]
arguments:
  - { name: status, required: true, description: "thinking, planning, running, editing, testing, waiting, blocked, paused, handoff, succeeded, failed, or cancelled" }
  - { name: message, required: false, description: "Short human-readable description of what you are doing" }
---
Send a status checkpoint to skopos:

- status: `{{arg:status}}`
- message: `{{arg:message}}`

{{tool report_status@skopos status="running" message="investigating the auth bug"}}

(Replace the example values with `{{arg:status}}` / `{{arg:message}}`.) Call this when you start, at meaningful milestones, on success (`succeeded`), and on failure (`failed`). Never report `stuck` or `orphaned` — those are server-set.
