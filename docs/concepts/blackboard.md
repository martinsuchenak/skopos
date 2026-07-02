# Blackboard

The blackboard is the shared memory layer — durable knowledge entries that agents write and read across sessions.

## Scopes

| Scope | Visibility | Requires |
|-------|-----------|----------|
| `session` | This session only | `session_id` |
| `branch` | All sessions on this git branch | `branch_name` |
| `project` | All agents, all branches | — |

Session-scoped entries are cascade-deleted when the session is deleted (via FK).

## Entry types

| Type | Floating? | Use case |
|------|-----------|----------|
| `bug` | ✅ (always visible across branches) | Critical issue every agent must see |
| `debt` | ✅ | Tech debt / cleanup item |
| `warning` | — | Gotcha, cautionary note |
| `finding` | — | Factual discovery |
| `decision` | — | Architectural choice |
| `context` | — | General project knowledge |

Floating entries (`bug`, `debt`) are returned in every bundle read regardless of branch filter — they're the "everyone needs to know" channel.

## Reading

**Full bundle** (`blackboard_read` MCP tool or `GET /api/blackboard/entries`):
Returns entries matching the scope rules + a pre-formatted markdown bundle. Parameters:
- `branch` — branch name (empty = all branches)
- `workspace_id` — workspace filter (strict match)
- `session_id` — include session-scoped entries

**Search** (`blackboard_read` with filter params, or query params on the REST endpoint):
- `entry_type` — filter by type
- `author` — filter by author_agent_id
- `q` — text search (title or content, LIKE match)

When any search filter is present, the tool returns matching entries (capped at 100, newest-first) instead of the scope-based bundle.

## Writing

`blackboard_write` MCP tool or `POST /api/blackboard/entries`. Required: `scope`, `entry_type`, `title`, `author_agent_id`.

Per-scope requirements:
- `scope=project` → `workspace_id` required
- `scope=branch` → `workspace_id` + `branch_name` required
- `scope=session` → `session_id` required (must reference an existing session — call `report_status` first)

## Promotion

Promote an entry to a wider scope: `session → branch → project`. Done via `PATCH /api/blackboard/entries/{id}/promote` or the dashboard's "Promote" button.

## Deletion

Permanently remove an entry with the `blackboard_delete` MCP tool (param: `id`) or `DELETE /api/blackboard/entries/{id}`. This is a **hard delete** — the row is removed immediately, and there is no archive/soft-delete for entries. Session-scoped entries are also removed automatically when their session is deleted (foreign-key cascade).

## Workspace scoping

When a workspace is selected, only entries with that exact `workspace_id` are returned. Entries with no `workspace_id` (global/unscoped) appear only under "All workspaces". See [Workspaces](workspaces.md).
