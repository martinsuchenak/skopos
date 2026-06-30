---
id: skopos-context
kind: command
description: Load skopos structural context for the current task — the branch's blackboard, active plans and blocked items, and in-flight sessions.
targets: [claude, opencode, codex, copilot, kiro]
arguments:
  - { name: branch, required: false, description: "Current git branch (defaults to the checked-out branch)" }
---
Load the skopos context bundle for the current branch (`{{arg:branch}}`):

{{tool skopos_context@skopos branch="feat-auth"}}

(Replace `feat-auth` with the actual current git branch.)

Use the returned snapshot to ground your work:
- **Blackboard:** prior findings/decisions/bugs for this branch — don't redo work or re-report known issues.
- **Plans:** active plans and any **blocked** items — pick these up or unblock dependents.
- **Sessions:** what else is currently in flight, to avoid clashing.
