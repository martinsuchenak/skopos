---
id: skopos-remember
kind: command
description: Write a finding, decision, bug, or other note to the skopos blackboard so other agent sessions can see it.
targets: [claude, opencode, codex, copilot, kiro]
arguments:
  - { name: type, required: true, description: "finding, decision, bug, debt, warning, or context" }
  - { name: title, required: true, description: "Short descriptive title" }
  - { name: scope, required: false, description: "branch (default), project, or session" }
---
Record a note on the skopos blackboard:

- entry_type: `{{arg:type}}`
- title: `{{arg:title}}`
- scope: `{{arg:scope}}` (default `branch`; use `project` for repo-wide; `bug`/`debt` float across all branches regardless of scope)

{{tool blackboard_write@skopos scope="branch" branch_name="feat-auth" entry_type="finding" title="short description" author_agent_id="claude-code-host"}}

(Replace the example values with your `{{arg:type}}`, `{{arg:title}}`, current branch, and a stable `author_agent_id`.) Add optional `content` (details) and `code_ref` (e.g. `auth/jwt.go:45`) when useful.
