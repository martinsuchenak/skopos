# Plans & Todo Lists Design

**Date:** 2026-05-13
**Status:** Approved
**Part of:** Skopos v1.2

## Summary

Add named, branch-scoped plan lists to Skopos so agents can coordinate work across sessions. A plan is a named checklist of items optionally tied to a git branch. Agents create plans at the start of work, claim and update individual items during execution, and leave the completed plan as a record for the next agent. Plans are exposed via REST, MCP tools, CLI, and a new web UI tab.

## Goals

- Let agents create named checklists scoped to a branch or project-wide.
- Let agents claim individual items (not whole plans) to signal "I'm working on this".
- Track item progress with four states: pending, in_progress, done, blocked.
- Multiple agents can work the same plan simultaneously on different items.
- Expose via MCP tools, REST API, CLI, and web dashboard.

## Non-Goals

- Plan-level assignment or locking.
- Dependencies between items.
- Automatic item generation from code analysis.
- Full-text search across plans.

## Architecture

A new `internal/plans` package follows the same `handler → service → storage` layering as `internal/blackboard` and `internal/status`. Uses the existing SQLite connection. No new infrastructure.

**Two tables:**
- `plans` — named plan, optionally scoped to a branch
- `plan_items` — ordered checklist items within a plan

`branch_name` is nullable on `plans`. A null branch means the plan is project-wide (visible to all agents regardless of branch).

**Item claiming:** `claimed_by_agent_id` is a nullable string on `plan_items`. Claiming is advisory — multiple agents can claim different items on the same plan simultaneously. No locking is enforced.

## Schema

```sql
CREATE TABLE IF NOT EXISTS plans (
    id                TEXT PRIMARY KEY,
    name              TEXT NOT NULL,
    branch_name       TEXT,               -- NULL = project-wide
    description       TEXT NOT NULL DEFAULT '',
    status            TEXT NOT NULL DEFAULT 'active', -- active | completed | archived
    author_agent_id   TEXT NOT NULL,
    created_at        TEXT NOT NULL,
    updated_at        TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS plan_items (
    id                  TEXT PRIMARY KEY,
    plan_id             TEXT NOT NULL,
    title               TEXT NOT NULL,
    description         TEXT NOT NULL DEFAULT '',
    status              TEXT NOT NULL DEFAULT 'pending', -- pending | in_progress | done | blocked
    position            INTEGER NOT NULL DEFAULT 0,
    claimed_by_agent_id TEXT,
    created_at          TEXT NOT NULL,
    updated_at          TEXT NOT NULL,
    FOREIGN KEY (plan_id) REFERENCES plans(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_plans_branch     ON plans(branch_name);
CREATE INDEX IF NOT EXISTS idx_plan_items_plan  ON plan_items(plan_id, position);
```

`ON DELETE CASCADE` on `plan_items.plan_id` means deleting a plan removes all its items.

## Package: `internal/plans/`

### `models.go`

```go
type PlanStatus string
type ItemStatus string

const (
    PlanActive    PlanStatus = "active"
    PlanCompleted PlanStatus = "completed"
    PlanArchived  PlanStatus = "archived"
)

const (
    ItemPending    ItemStatus = "pending"
    ItemInProgress ItemStatus = "in_progress"
    ItemDone       ItemStatus = "done"
    ItemBlocked    ItemStatus = "blocked"
)

type Plan struct {
    ID            string     `json:"id"`
    Name          string     `json:"name"`
    BranchName    string     `json:"branch_name,omitempty"`
    Description   string     `json:"description,omitempty"`
    Status        PlanStatus `json:"status"`
    AuthorAgentID string     `json:"author_agent_id"`
    Items         []Item     `json:"items,omitempty"`
    CreatedAt     time.Time  `json:"created_at"`
    UpdatedAt     time.Time  `json:"updated_at"`
}

type Item struct {
    ID                string     `json:"id"`
    PlanID            string     `json:"plan_id"`
    Title             string     `json:"title"`
    Description       string     `json:"description,omitempty"`
    Status            ItemStatus `json:"status"`
    Position          int        `json:"position"`
    ClaimedByAgentID  string     `json:"claimed_by_agent_id,omitempty"`
    CreatedAt         time.Time  `json:"created_at"`
    UpdatedAt         time.Time  `json:"updated_at"`
}

var (
    ErrInvalidInput   = errors.New("invalid plans input")
    ErrNotFound       = errors.New("not found")
)

type CreatePlanInput struct {
    Name          string `json:"name"`
    BranchName    string `json:"branch_name,omitempty"`
    Description   string `json:"description,omitempty"`
    AuthorAgentID string `json:"author_agent_id"`
}

type UpdatePlanInput struct {
    Name        string     `json:"name,omitempty"`
    Description string     `json:"description,omitempty"`
    Status      PlanStatus `json:"status,omitempty"`
}

type CreateItemInput struct {
    Title       string `json:"title"`
    Description string `json:"description,omitempty"`
    Position    *int   `json:"position,omitempty"` // appended at end if nil
}

type UpdateItemInput struct {
    Status           ItemStatus `json:"status,omitempty"`
    ClaimedByAgentID *string    `json:"claimed_by_agent_id"` // pointer: null = release claim
}
```

### `storage.go`

```go
type Store interface {
    CreatePlan(ctx context.Context, plan Plan) error
    GetPlan(ctx context.Context, id string) (*Plan, error)
    ListPlans(ctx context.Context, branchName string) ([]Plan, error)
    UpdatePlan(ctx context.Context, id string, input UpdatePlanInput) error
    DeletePlan(ctx context.Context, id string) error

    AddItem(ctx context.Context, item Item) error
    UpdateItem(ctx context.Context, planID, itemID string, input UpdateItemInput) error
    DeleteItem(ctx context.Context, planID, itemID string) error
}
```

`GetPlan` returns the plan with all items populated (ordered by `position ASC, created_at ASC`).

`ListPlans` returns plans matching `branch_name = ?` OR `branch_name IS NULL` (project-wide plans always included), without items. Results ordered by `created_at DESC`.

`UpdateItem` applies only non-zero fields. For `ClaimedByAgentID`, a nil pointer means "don't change", a pointer to empty string means "release claim".

### `service.go`

Validates all inputs:
- `CreatePlanInput`: `name` and `author_agent_id` required.
- `CreateItemInput`: `title` required.
- `UpdatePlanInput`: if `status` provided, must be valid (`active`, `completed`, `archived`).
- `UpdateItemInput`: if `status` provided, must be valid (`pending`, `in_progress`, `done`, `blocked`).

Position for new items defaults to `len(existing items)` (appended at end) when `Position` is nil.

### `handler.go`

Standard REST handler following `internal/blackboard/handler.go` pattern. Auth middleware on all write endpoints.

## REST API

All endpoints under `/api/plans`.

| Method | Path | Auth | Description |
|--------|------|------|-------------|
| `POST` | `/api/plans` | required | Create a plan |
| `GET` | `/api/plans` | none | List plans (`?branch=feat-auth`, omit for all) |
| `GET` | `/api/plans/{id}` | none | Get plan with all items |
| `PATCH` | `/api/plans/{id}` | required | Update plan name/description/status |
| `DELETE` | `/api/plans/{id}` | required | Delete plan and all items |
| `POST` | `/api/plans/{id}/items` | required | Add item to plan |
| `PATCH` | `/api/plans/{id}/items/{item_id}` | required | Update item status / claim |
| `DELETE` | `/api/plans/{id}/items/{item_id}` | required | Remove item |

**POST /api/plans body:**
```json
{
  "name": "Auth refactor",
  "branch_name": "feat-auth",
  "description": "Optional overview",
  "author_agent_id": "claude-code-macbook"
}
```

**GET /api/plans response:**
```json
[
  { "id": "...", "name": "Auth refactor", "branch_name": "feat-auth",
    "status": "active", "author_agent_id": "...", "created_at": "..." }
]
```
(no items in list response — fetch individual plan for items)

**GET /api/plans/{id} response:**
```json
{
  "id": "...", "name": "Auth refactor", "branch_name": "feat-auth",
  "status": "active", "author_agent_id": "...",
  "items": [
    { "id": "...", "title": "Audit refresh token logic", "status": "in_progress",
      "position": 0, "claimed_by_agent_id": "claude-code-macbook", ... }
  ]
}
```

**PATCH /api/plans/{id}/items/{item_id} body:**
```json
{ "status": "done" }
{ "status": "in_progress", "claimed_by_agent_id": "codex-mbp" }
{ "claimed_by_agent_id": "" }
```

## MCP Tools

### `plan_create`

Parameters: `name` (required), `author_agent_id` (required), `branch_name` (optional), `description` (optional).

Returns: `{ "id": "...", "name": "..." }`

### `plan_read`

Parameters: `id` (required).

Returns: full `Plan` with items.

### `plan_update_item`

Parameters: `plan_id` (required), `item_id` (required), `status` (optional), `claimed_by_agent_id` (optional).

Returns: updated `Item`.

### `plan_add_item`

Parameters: `plan_id` (required), `title` (required), `description` (optional).

Returns: created `Item`.

## CLI

```
skopos plan create  --name "Auth refactor" [--branch feat-auth] [--description "..."] [--agent-id "..."]
skopos plan list    [--branch feat-auth]
skopos plan show    <id>
skopos plan done    <id>          # marks plan status=completed
skopos plan item add   <plan-id> --title "..." [--description "..."]
skopos plan item done  <plan-id> <item-id>
skopos plan item claim <plan-id> <item-id> [--agent-id "..."]  # defaults to $SKOPOS_AGENT_ID
skopos plan item unclaim <plan-id> <item-id>
skopos plan item block <plan-id> <item-id>
```

## Web UI

New **Plans** tab in the nav alongside Sessions and Blackboard.

- Branch filter input (same pattern as Blackboard tab).
- Plans listed as cards: name, branch badge (or "project-wide"), status badge, item progress bar (`done/total`).
- Clicking a plan expands (or navigates to) its item checklist.
- Items shown with status badge, claimed-by label, checkbox-style done indicator.
- Auto-refresh every 5 seconds.

## Wiring

In `cmd/serve.go`:
```go
plansStorage := plans.NewStorage(conn.SQL)
plansService := plans.NewService(plansStorage)
plansHandler := plans.NewHandler(plansService, cmd.GetString("api-key"))
```

Registered via `cmd/routes/plans_routes.go` (same `init()` pattern) and `cmd/mcp/plan_*.go` tools.

## Test Plan

- `internal/plans/storage_test.go` — real `:memory:` SQLite:
  - Create + get plan with items
  - List plans: branch filter includes project-wide, excludes other branches
  - Cascade delete removes items when plan deleted
  - Update item status and claim
  - Release claim (empty string)
  - Delete item

- `internal/plans/service_test.go` — fake store:
  - Validation: missing name, missing author_agent_id, invalid status values
  - Position defaults to end of list
  - UpdateItemInput: nil pointer vs pointer-to-empty for claim

- `internal/plans/handler_test.go` — real SQLite:
  - POST plan → 201, GET plan → 200 with items
  - PATCH item status → 204
  - DELETE plan cascades → 404 on item fetch
  - Auth required on writes

- `cmd/mcp/*_test.go` — registration checks (same pattern as blackboard)

Run: `task test` covers everything.
