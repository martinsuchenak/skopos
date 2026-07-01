# Workspaces

Workspaces scope sessions, blackboard entries, and plans to a logical unit (typically a repository or project).

## What is a workspace?

A workspace is identified by a string (e.g. `github.com/you/repo` or `/path/to/project`). Agents report with a `workspace` field; blackboard entries and plans carry a `workspace_id`. When filtering by a workspace, only items with that exact `workspace_id` are returned.

## Registry

Workspaces can be explicitly registered (`POST /api/workspaces`) with an optional display name. The dashboard's workspace picker shows registered workspaces (with names) plus any workspace inferred from session data.

**Auto-registration**: any workspace seen in session data is automatically registered so it persists in the database even if all sessions are later deleted. This prevents workspaces from vanishing from the picker when their data is cleaned up.

## Strict scoping

When a workspace filter is active (a workspace is selected in the picker or passed as a query parameter), blackboard entries and plans require an **exact** `workspace_id` match:

- Items with no `workspace_id` (unscoped/global) appear **only** under "All workspaces".
- Items created from the dashboard while a workspace is selected are automatically stamped with that `workspace_id`.

## UI behavior

- **Workspace picker** (sidebar) — shows registered workspaces ∪ session-derived workspaces.
- **"New" button** — opens a modal to register a workspace (ID + optional name).
- **Workspace badges** — under "All workspaces", each blackboard entry and plan shows a workspace badge (sky) or "no workspace" (muted) so you can see what belongs where.
- **Switching workspaces** — immediately refreshes the active view (sessions, blackboard, plans) with the new workspace's data.

## REST endpoints

| Method | Path | Description |
|--------|------|-------------|
| POST | `/api/workspaces` | Register/upsert a workspace |
| GET | `/api/workspaces` | List registered workspaces |
| DELETE | `/api/workspaces/{id}` | Unregister (does not delete data) |
