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
