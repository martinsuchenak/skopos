## Skopos Status Reporting

Report your status to the Skopos dashboard via the `skopos__report_status` MCP tool throughout your work.

**When starting a session or beginning a task:**
Call `skopos__report_status` with:
- `agent_id`: `"github-copilot-<hostname>"` (replace with actual hostname)
- `agent_type`: `"github-copilot"`
- `workspace`: the current workspace folder path
- `status`: `"running"`
- `message`: `"session started"` or a brief description of the task

**During work:**
Optionally call `skopos__report_status` with accurate status values to reflect what you are doing:
- `thinking` — analysing the problem
- `planning` — designing an approach
- `editing` — writing or modifying code
- `testing` — running or reviewing tests
- `blocked` — waiting for clarification or missing information

**When completing a task successfully:**
Call `skopos__report_status` with `status: "succeeded"`, `message: "task complete"`.

**When completing with errors or unable to proceed:**
Call `skopos__report_status` with `status: "failed"`, `message: "task ended with error"`.

Available status values: `pending`, `thinking`, `planning`, `running`, `editing`, `testing`, `waiting`, `blocked`, `paused`, `handoff`, `succeeded`, `failed`, `cancelled`.

## Skopos Blackboard

Use the blackboard to share knowledge with other agent sessions.

**At the start of a task:** Call `skopos__blackboard_read` with `branch` set to the current git branch to load prior findings.

**When you discover something worth recording:** Call `skopos__blackboard_write` with:

- `scope`: "branch" (shared on this branch) or "project" (all agents)
- `branch_name`: current branch (required when scope is "branch")
- `entry_type`: "finding", "decision", "bug", "debt", "warning", or "context"
- `title`: short description
- `content`: details (optional)
- `code_ref`: file and line reference (optional)
- `author_agent_id`: "github-copilot-HOSTNAME"

Use `entry_type: "bug"` or `"debt"` for critical issues — these are always visible to all agents regardless of branch.
