# Blackboard Memory Design

**Date:** 2026-05-12
**Status:** Approved
**Part of:** Skopos v1.1

## Summary

Add a scoped shared memory system to Skopos that lets AI agents leave structured notes for one another, enabling handoff without rediscovery. A new `internal/blackboard` package stores entries in SQLite at three scopes (session, branch, project). Agents read a filtered "Knowledge Bundle" — a structured JSON response plus a formatted markdown text block — tailored to their current branch and session context. Two "floating" entry types (`bug`, `debt`) are always served regardless of branch, enabling cross-branch warnings.

## Goals

- Let one agent finish work and leave structured notes that the next agent can consume immediately.
- Scope memory so branch-specific discoveries don't contaminate other branches.
- Allow critical findings (bugs, tech debt) to float above branch restrictions and reach all agents.
- Expose blackboard via MCP tools, REST API, and CLI — covering all existing agent integrations.
- Keep the implementation local-first: SQLite only, no new infrastructure dependencies.

## Non-Goals

- Stale-fact validation hooks (verifying that code references still exist) — deferred to phase 2.
- Automatic promotion on git merge.
- Dashboard UI for blackboard entries (API-only for this phase).
- Full-text search across entries.

## Architecture

A new `internal/blackboard` package follows the same `handler → service → storage` layering as `internal/status`. It uses the existing SQLite connection (`conn.SQL`) passed in from `serve.go`. No second database, no Valkey dependency.

**Three memory scopes:**
- `session` — visible only within the current Skopos session; auto-deleted via `ON DELETE CASCADE` when the session ends.
- `branch` — shared between any agents reporting the same `branch_name`; persists until explicitly deleted or promoted.
- `project` — global; visible to all agents regardless of branch.

**Floating types:** Entries with `entry_type = 'bug'` or `entry_type = 'debt'` are always included in the Knowledge Bundle regardless of their scope or branch. This allows a critical bug discovered on a feature branch to immediately warn agents on other branches.

**Promotion lifecycle:** `session → branch → project`. Promotion is a single PATCH that increments the scope and nulls `branch_name` when reaching project level. Demotion is not supported.

## Schema

Added to `internal/db/schema.sql` (no migration — DB is wiped and recreated from `schema.sql`):

```sql
CREATE TABLE IF NOT EXISTS blackboard_entries (
    id              TEXT PRIMARY KEY,
    scope           TEXT NOT NULL,      -- 'session' | 'branch' | 'project'
    branch_name     TEXT,               -- NULL when scope = 'project'
    session_id      TEXT,               -- NULL when scope = 'branch' | 'project'
    entry_type      TEXT NOT NULL,      -- 'finding' | 'decision' | 'bug' | 'debt' | 'warning' | 'context'
    title           TEXT NOT NULL,
    content         TEXT NOT NULL DEFAULT '',
    code_ref        TEXT,               -- optional, e.g. 'auth/jwt.go:45' or 'validateToken()'
    author_agent_id TEXT NOT NULL,
    created_at      TEXT NOT NULL,
    updated_at      TEXT NOT NULL,
    FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_blackboard_scope   ON blackboard_entries(scope, branch_name);
CREATE INDEX IF NOT EXISTS idx_blackboard_session ON blackboard_entries(session_id);
CREATE INDEX IF NOT EXISTS idx_blackboard_type    ON blackboard_entries(entry_type);
```

The `ON DELETE CASCADE` on `session_id` handles session-scoped entry cleanup automatically — no background job needed.

`git_branch` is added as a new optional field to `ReportInput` and `AgentState`. When an agent reports status it can declare its current branch, making it available without a separate lookup.

## Package: `internal/blackboard/`

### `models.go`

```go
type Scope     string
type EntryType string

const (
    ScopeSession Scope = "session"
    ScopeBranch  Scope = "branch"
    ScopeProject Scope = "project"
)

const (
    TypeFinding  EntryType = "finding"
    TypeDecision EntryType = "decision"
    TypeBug      EntryType = "bug"    // floating — always cross-branch
    TypeDebt     EntryType = "debt"   // floating — always cross-branch
    TypeWarning  EntryType = "warning"
    TypeContext  EntryType = "context"
)

type Entry struct {
    ID            string    `json:"id"`
    Scope         Scope     `json:"scope"`
    BranchName    string    `json:"branch_name,omitempty"`
    SessionID     string    `json:"session_id,omitempty"`
    EntryType     EntryType `json:"entry_type"`
    Title         string    `json:"title"`
    Content       string    `json:"content"`
    CodeRef       string    `json:"code_ref,omitempty"`
    AuthorAgentID string    `json:"author_agent_id"`
    CreatedAt     time.Time `json:"created_at"`
    UpdatedAt     time.Time `json:"updated_at"`
}

var (
    ErrInvalidInput     = errors.New("invalid blackboard input")
    ErrNotFound         = errors.New("not found")
    ErrAlreadyAtTopScope = errors.New("entry is already at project scope")
)

type WriteInput struct {
    Scope         Scope     `json:"scope"`
    BranchName    string    `json:"branch_name,omitempty"`
    SessionID     string    `json:"session_id,omitempty"`
    EntryType     EntryType `json:"entry_type"`
    Title         string    `json:"title"`
    Content       string    `json:"content,omitempty"`
    CodeRef       string    `json:"code_ref,omitempty"`
    AuthorAgentID string    `json:"author_agent_id"`
}

type Bundle struct {
    Entries        []Entry `json:"entries"`
    MarkdownBundle string  `json:"markdown_bundle"` // formatted text for pasting into agent context
}
```

### `storage.go`

```go
type Store interface {
    Write(ctx context.Context, entry Entry) error
    Bundle(ctx context.Context, branchName, sessionID string) (*Bundle, error)
    Promote(ctx context.Context, id string) error
    Delete(ctx context.Context, id string) error
    Get(ctx context.Context, id string) (*Entry, error)
}
```

**`Bundle` query:**
```sql
SELECT * FROM blackboard_entries
WHERE scope = 'project'
   OR (scope = 'branch' AND branch_name = ?)
   OR (scope = 'session' AND session_id = ?)
   OR entry_type IN ('bug', 'debt')
ORDER BY entry_type, created_at ASC
```

**`Promote` logic:** reads current scope, advances it (`session → branch → project`), nulls `branch_name` when reaching project. Returns `ErrNotFound` if entry doesn't exist, `ErrAlreadyAtTopScope` if already project.

### `service.go`

Validates `WriteInput`: required fields (`scope`, `entry_type`, `title`, `author_agent_id`), valid scope value, valid entry_type value, `branch_name` required when `scope = 'branch'`, `session_id` required when `scope = 'session'`.

`MarkdownBundle` is assembled in the service layer from the query result, grouped by type:

```
## Skopos Knowledge Bundle
### Branch: feat-auth  |  Project: skopos

#### 🐛 Bugs (cross-branch)
- **JWT expiry not checked on refresh** (auth/jwt.go:45)
  Found by claude-code-macbook. Refresh tokens bypass expiry validation.

#### 🔍 Findings
- **DB index missing on agent_states.session_id** (schema.sql:22)
  ...
```

### `handler.go`

Standard REST handler following the `internal/status/handler.go` pattern. Auth middleware applied to write/promote/delete endpoints via the existing `auth` package.

## REST API

All endpoints under `/api/blackboard`.

| Method | Path | Auth | Description |
|--------|------|------|-------------|
| `POST` | `/api/blackboard/entries` | required | Write an entry |
| `GET` | `/api/blackboard/entries` | none | Read Knowledge Bundle |
| `PATCH` | `/api/blackboard/entries/{id}/promote` | required | Promote scope up one level |
| `DELETE` | `/api/blackboard/entries/{id}` | required | Delete an entry |

**GET query params:** `branch` (branch name), `session_id` (optional). Both may be omitted to get only project-scope + floating entries.

**POST body:**
```json
{
  "scope": "branch",
  "branch_name": "feat-auth",
  "entry_type": "finding",
  "title": "JWT expiry not checked on refresh",
  "content": "Refresh tokens bypass expiry validation entirely.",
  "code_ref": "auth/jwt.go:45",
  "author_agent_id": "claude-code-macbook"
}
```

**GET response:**
```json
{
  "entries": [...],
  "markdown_bundle": "## Skopos Knowledge Bundle\n..."
}
```

## MCP Tools

Two new tools added to the existing MCP server (`cmd/mcp/`):

### `blackboard_write`

Parameters: `scope`, `branch_name` (optional), `session_id` (optional), `entry_type`, `title`, `content` (optional), `code_ref` (optional), `author_agent_id`.

Returns: `{ "id": "...", "scope": "branch" }`

### `blackboard_read`

Parameters: `branch` (optional), `session_id` (optional).

Returns: the full `Bundle` — structured `entries` array plus `markdown_bundle` text field.

## CLI

New `blackboard` subcommand group registered in `cmd/`:

```
skopos blackboard write   --scope branch --branch feat-auth --type finding --title "..." --content "..." [--code-ref "auth/jwt.go:45"] [--agent-id "..."]
skopos blackboard read    [--branch feat-auth] [--session-id "..."]   # prints markdown_bundle to stdout
skopos blackboard list    [--branch feat-auth]                         # tabular list with id, type, scope, title
skopos blackboard promote <id>
skopos blackboard delete  <id>
```

`--agent-id` defaults to `$SKOPOS_AGENT_ID` env var if set.

## `git_branch` on ReportInput

`git_branch string` added as an optional field to `ReportInput` and `AgentState`. Stored in the `agent_states` table as a new nullable column `git_branch TEXT`. Agents that report their branch via `POST /api/reports` or the MCP `report_status` tool automatically populate this field; the blackboard read tool can then infer the branch from the agent's current state if not passed explicitly.

## Wiring

In `cmd/serve.go`:

```go
blackboardStorage := blackboard.NewStorage(conn.SQL)
blackboardService := blackboard.NewService(blackboardStorage)
blackboardHandler := blackboard.NewHandler(blackboardService, cmd.GetString("api-key"))
// registered in routes.RegisterRoutes
```

No new flags or config sections needed.

## Test Plan

- `internal/blackboard/storage_test.go` — real `:memory:` SQLite + `db.RunMigrations`:
  - Write + read bundle (session, branch, project scopes)
  - Floating types (`bug`, `debt`) appear in bundles regardless of branch filter
  - `ON DELETE CASCADE` removes session entries when session is deleted
  - Promote: session→branch, branch→project, project returns error
  - Delete removes entry

- `internal/blackboard/service_test.go` — fake store:
  - Validation: missing required fields, invalid scope, invalid entry_type
  - `branch_name` required for branch scope
  - `session_id` required for session scope
  - `MarkdownBundle` formatting produces non-empty output

- `cmd/mcp/blackboard_*_test.go` — accepted/rejected payloads for both MCP tools

- Run: `task test` covers everything
