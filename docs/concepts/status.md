# Status

Agents report their status to skopos, which tracks sessions, individual agents, and an event timeline.

## Reporting

Agents call `report_status` (MCP) or `POST /api/reports` (REST) with:

- `agent_id` (required) — stable identifier (e.g. `codex-macbook`)
- `agent_type` (required) — `codex`, `claude-code`, `gemini`, `opencode`, `kiro`, etc.
- `workspace_id` (required) — workspace ID or repository path
- `status` (required) — one of the valid statuses below
- `session_id` (optional) — share a session across agents; auto-generated if omitted
- `progress`, `step_current`, `step_total` (optional) — progress tracking
- `message`, `snippet` (optional) — human-readable status
- `git_branch` (optional) — current git branch

## Valid statuses

**Agent-reportable** (13):
`pending`, `thinking`, `planning`, `running`, `editing`, `testing`, `waiting`, `blocked`, `paused`, `handoff`, `succeeded`, `failed`, `cancelled`

**Server-set only** (2) — agents must NOT report these:
`stuck`, `orphaned`

Invalid statuses are rejected with: `unsupported status "X". Valid statuses: pending, thinking, planning, running, ...`

## Health checker

A background goroutine (every 60s) detects:
- **Stuck agents** — agents in an active status (`pending`, `running`, `thinking`, etc.) that haven't updated within `health-stuck-threshold` minutes (default 15). The checker sets `status=stuck`, records the original status, and creates a `stuck` event.
- **Orphaned sessions** — sessions where all agents are in terminal/stuck states. Marked `orphaned` for cleanup.

## Cleanup worker

A background goroutine (every 10 min) deletes data older than `cleanup-retention-days` (default 30):
- Events older than retention
- Orphaned sessions older than retention
- Session-scoped blackboard entries whose session no longer exists
- Completed/archived plans older than retention
- Agents not seen since retention

Set `--cleanup-retention-days 0` to disable.

## Session lifecycle

```
report(running)  →  session created/updated, agent registered
report(succeeded) →  session status = succeeded
report(failed)    →  session status = failed
health checker    →  marks stuck/orphaned if no updates
cleanup worker    →  deletes old data after retention
```

Sessions can be deleted via `DELETE /api/sessions/{id}` (requires API key). Deleting a session cascades to its agents, events, and session-scoped blackboard entries (via FK).
