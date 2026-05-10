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
