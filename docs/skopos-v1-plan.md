# Skopos V1 Agent Status Hub

## Summary

Build Skopos as a local-first central dashboard where multiple AI agents report status into shared sessions. V1 will replace the generated sample domain with a real status/progress model, using REST as the primary ingestion API, a CLI wrapper for universal agent compatibility, SQLite persistence, and a polling Alpine/Tailwind dashboard.

## Key Changes

- Add a core domain for `sessions`, `agents`, and `events`.
- Agents self-report with `agent_id`, `agent_type`, `workspace`, optional `session_id`, status, progress, message, output snippet, and metadata JSON.
- Missing sessions are created implicitly on first report.
- Use a controlled rich status set: `pending`, `thinking`, `planning`, `running`, `editing`, `testing`, `waiting`, `blocked`, `paused`, `handoff`, `succeeded`, `failed`, `cancelled`.
- Store summaries and snippets only, not full transcripts/log streams.

## Interfaces

- REST:
  - `POST /api/reports`: authenticated write endpoint for agent status/progress reports.
  - `GET /api/sessions`: list sessions with current aggregate status and active agents.
  - `GET /api/sessions/{id}`: session detail with latest agent states.
  - `GET /api/sessions/{id}/events`: chronological event feed.
- CLI:
  - `skopos report --agent-id ... --agent-type ... --status ... --message ...`
  - Support `--session-id`, `--workspace`, `--progress`, `--step-current`, `--step-total`, `--snippet`, and `--metadata`.
  - CLI sends to REST so Gemini, Codex, Claude Code, OpenCode, Kiro, and Praxis can all shell out to the same reporting path.
- MCP:
  - Replace the sample tool with a `report_status` tool that mirrors the REST report payload for MCP-capable agents.
- Auth:
  - Require a single configured API key for write/report endpoints via `X-API-Key`.
  - Add config/env support for `auth.api_key` / `SKOPOS_API_KEY`.

## Implementation Shape

- Keep generated layering intact: HTTP handler -> service -> SQLite storage.
- Replace `internal/sample` and sample routes/tools with an `internal/status` domain.
- Update `internal/db/schema.sql` with durable tables and indexes for sessions, agent latest state, and append-only events.
- Wire the database connection and migrations into `serve`, using existing `database.path`.
- Update `openapi.yaml` to document the new endpoints and request/response shapes.
- Build the dashboard in `web/` as the first screen: session list, active agent cards/table, progress indicators, latest message/snippet, and event timeline. Use polling, not SSE/WebSockets.

## Test Plan

- Unit test service validation for required fields, status validation, progress range, and implicit session creation.
- Storage tests for creating sessions, upserting latest agent state, appending events, and querying session summaries/events.
- Handler tests for valid reports, auth failures, malformed JSON, invalid status/progress, and read endpoints.
- CLI tests for argument parsing and report payload construction.
- MCP tool tests for accepted/rejected report payloads.
- Run `task test`, and where practical `task build`, `task lint`, and `task frontend-build`.

## Progress

- [x] Initial V1 plan written to this document.
- [x] SQLite schema and status domain storage/service implemented.
- [x] REST routes, auth, DB startup, and OpenAPI wired.
- [x] CLI `report` command and MCP `report_status` tool added.
- [x] Polling dashboard UI built.
- [x] Tests/build checks run.

## Assumptions

- V1 is local-first/single-instance; no multi-user accounts or per-agent registration yet.
- Polling is acceptable for the dashboard.
- API reads can remain unauthenticated initially; writes require the single API key.
- Full logs/transcripts are out of scope for v1 to avoid storing sensitive data by default.
