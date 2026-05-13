## Skopos Status Reporting

Report your status to the Skopos dashboard when starting and completing work.

**When starting a session:**
Call the `skopos__report_status` MCP tool with:
- `agent_id`: `"kiro-<hostname>"` (replace `<hostname>` with actual hostname)
- `agent_type`: `"kiro"`
- `workspace`: the current working directory
- `status`: `"running"`
- `message`: `"session started"`

**When completing a session successfully:**
Call `skopos__report_status` with the same agent_id/agent_type/workspace, status `"succeeded"`, message `"session complete"`.

**When completing with errors:**
Call `skopos__report_status` with status `"failed"`, message `"session ended with error"`.

**During work:**
Optionally call `skopos__report_status` with accurate status values (`thinking`, `planning`, `editing`, `testing`, `blocked`) and a short message describing what you are doing.

Available status values: `pending`, `thinking`, `planning`, `running`, `editing`, `testing`, `waiting`, `blocked`, `paused`, `handoff`, `succeeded`, `failed`, `cancelled`.

## Skopos Blackboard

Use the blackboard to share knowledge with other agent sessions.

**At the start of a session:** Call `skopos__blackboard_read` with `branch` set to the current git branch to load prior findings.

**When you discover something worth recording:** Call `skopos__blackboard_write` with:

- `scope`: "branch" (shared on this branch) or "project" (all agents)
- `branch_name`: current branch (required when scope is "branch")
- `entry_type`: "finding", "decision", "bug", "debt", "warning", or "context"
- `title`: short description
- `content`: details (optional)
- `code_ref`: file and line reference (optional)
- `author_agent_id`: "kiro-HOSTNAME"

Use `entry_type: "bug"` or `"debt"` for critical issues — these are always visible to all agents regardless of branch.
