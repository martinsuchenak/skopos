# Blackboard Memory Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a scoped shared memory system (`internal/blackboard`) so AI agents can leave structured notes for one another, served via REST, MCP tools, and CLI.

**Architecture:** A new `internal/blackboard` package follows the same `handler → service → storage` layering as `internal/status`. All data lives in a single `blackboard_entries` SQLite table; entries are filtered by scope (session/branch/project) plus floating types (bug/debt always cross-branch). The Knowledge Bundle — structured JSON plus a formatted markdown text block — is assembled in the service layer.

**Tech Stack:** Go 1.26, `modernc.org/sqlite` (via `database/sql`), `github.com/paularlott/mcp`, `github.com/paularlott/cli`, `github.com/google/uuid`, standard library `net/http`.

---

## File Map

**Create:**
- `internal/blackboard/models.go` — types, constants, error vars, WriteInput/WriteResult/Bundle structs
- `internal/blackboard/storage.go` — Store interface + SQLite implementation
- `internal/blackboard/storage_test.go` — real `:memory:` SQLite tests
- `internal/blackboard/service.go` — validation, MarkdownBundle assembly
- `internal/blackboard/service_test.go` — fake-store tests
- `internal/blackboard/handler.go` — REST handler (WriteEntry, ReadBundle, Promote, Delete)
- `internal/blackboard/handler_test.go` — HTTP handler tests
- `cmd/routes/blackboard_routes.go` — route registration via init()
- `cmd/routes/blackboard_routes_test.go` — smoke test
- `cmd/mcp/blackboard_write_tool.go` — MCP blackboard_write tool
- `cmd/mcp/blackboard_write_tool_test.go` — registration check
- `cmd/mcp/blackboard_read_tool.go` — MCP blackboard_read tool
- `cmd/mcp/blackboard_read_tool_test.go` — registration check
- `cmd/blackboard.go` — `skopos blackboard` CLI command group
- `cmd/blackboard_test.go` — command existence check

**Modify:**
- `internal/db/schema.sql` — add `blackboard_entries` table + indexes + `git_branch TEXT` column on `agent_states`
- `internal/status/models.go` — add `GitBranch` to `ReportInput` and `AgentState`
- `internal/status/storage.go` — update upsert + scan for `git_branch`
- `internal/status/storage_test.go` — add `git_branch` round-trip test
- `cmd/routes/api_routes.go` — add `blackboardRegistrations` + update `RegisterRoutes` signature
- `cmd/mcp/mcp.go` — add `blackboardToolRegistrations` + update `StartMCPServer` signature
- `cmd/serve.go` — wire `blackboard.Storage/Service/Handler`, pass to routes and MCP

---

## Task 1: Schema changes

**Files:**
- Modify: `internal/db/schema.sql`

- [ ] **Step 1: Add `git_branch` column to `agent_states` and the `blackboard_entries` table**

Open `internal/db/schema.sql`. After the `stuck_at TEXT` line in `agent_states`, add:

```sql
    git_branch      TEXT,
```

So the full `agent_states` block ends with:
```sql
    original_status TEXT,
    stuck_at        TEXT,
    git_branch      TEXT,
    PRIMARY KEY (session_id, agent_id),
    FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE CASCADE,
    FOREIGN KEY (agent_id) REFERENCES agents(id) ON DELETE CASCADE
);
```

Then, after the existing `CREATE INDEX` statements, append the full blackboard schema:

```sql
CREATE TABLE IF NOT EXISTS blackboard_entries (
    id              TEXT PRIMARY KEY,
    scope           TEXT NOT NULL,
    branch_name     TEXT,
    session_id      TEXT,
    entry_type      TEXT NOT NULL,
    title           TEXT NOT NULL,
    content         TEXT NOT NULL DEFAULT '',
    code_ref        TEXT,
    author_agent_id TEXT NOT NULL,
    created_at      TEXT NOT NULL,
    updated_at      TEXT NOT NULL,
    FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_blackboard_scope   ON blackboard_entries(scope, branch_name);
CREATE INDEX IF NOT EXISTS idx_blackboard_session ON blackboard_entries(session_id);
CREATE INDEX IF NOT EXISTS idx_blackboard_type    ON blackboard_entries(entry_type);
```

- [ ] **Step 2: Verify schema compiles**

```bash
go build ./internal/db/...
```

Expected: no output (success).

- [ ] **Step 3: Commit**

```bash
git add internal/db/schema.sql
git commit -m "feat(schema): add blackboard_entries table and git_branch to agent_states"
```

---

## Task 2: `git_branch` on status models and storage

**Files:**
- Modify: `internal/status/models.go`
- Modify: `internal/status/storage.go`
- Modify: `internal/status/storage_test.go`

- [ ] **Step 1: Write the failing test** in `internal/status/storage_test.go`

Add at the end of the file:

```go
func TestStorageRecordReportStoresGitBranch(t *testing.T) {
	storage := testStorage(t)
	ctx := context.Background()

	err := storage.RecordReport(ctx, Event{
		ID:        "event-1",
		SessionID: "session-1",
		AgentID:   "agent-1",
		AgentType: "codex",
		Workspace: "/repo",
		Status:    StatusRunning,
		Message:   "working",
		Metadata:  map[string]any{},
		CreatedAt: time.Now(),
	}, "/repo", "feat-auth")
	if err != nil {
		t.Fatalf("record report: %v", err)
	}

	detail, err := storage.GetSession(ctx, "session-1")
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if len(detail.Agents) != 1 {
		t.Fatalf("expected 1 agent, got %d", len(detail.Agents))
	}
	if detail.Agents[0].GitBranch != "feat-auth" {
		t.Errorf("git_branch: got %q, want %q", detail.Agents[0].GitBranch, "feat-auth")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
go test ./internal/status/... -run TestStorageRecordReportStoresGitBranch -v
```

Expected: FAIL — compilation error about `GitBranch` not existing and `RecordReport` signature mismatch.

- [ ] **Step 3: Add `GitBranch` to models**

In `internal/status/models.go`, add `GitBranch` to `ReportInput` (after `Metadata`):

```go
type ReportInput struct {
	SessionID   string         `json:"session_id,omitempty"`
	AgentID     string         `json:"agent_id"`
	AgentType   string         `json:"agent_type"`
	Workspace   string         `json:"workspace"`
	Status      Status         `json:"status"`
	Progress    *int           `json:"progress,omitempty"`
	StepCurrent *int           `json:"step_current,omitempty"`
	StepTotal   *int           `json:"step_total,omitempty"`
	Message     string         `json:"message,omitempty"`
	Snippet     string         `json:"snippet,omitempty"`
	Metadata    map[string]any `json:"metadata,omitempty"`
	GitBranch   string         `json:"git_branch,omitempty"`
}
```

Add `GitBranch` to `AgentState` (after `StuckAt`):

```go
type AgentState struct {
	SessionID      string         `json:"session_id"`
	AgentID        string         `json:"agent_id"`
	AgentType      string         `json:"agent_type"`
	Workspace      string         `json:"workspace"`
	Status         Status         `json:"status"`
	Progress       *int           `json:"progress,omitempty"`
	StepCurrent    *int           `json:"step_current,omitempty"`
	StepTotal      *int           `json:"step_total,omitempty"`
	Message        string         `json:"message"`
	Snippet        string         `json:"snippet"`
	Metadata       map[string]any `json:"metadata"`
	UpdatedAt      time.Time      `json:"updated_at"`
	OriginalStatus *Status        `json:"original_status,omitempty"`
	StuckAt        *time.Time     `json:"stuck_at,omitempty"`
	GitBranch      string         `json:"git_branch,omitempty"`
}
```

- [ ] **Step 4: Update `storage.go` — add `nullableString` helper and `git_branch` support**

Add `nullableString` at the bottom of `internal/status/storage.go` (after `parseTime`):

```go
func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
```

Update `RecordReport` — change the `agent_states` INSERT to include `git_branch`:

```go
if _, err := tx.ExecContext(ctx, `
    INSERT INTO agent_states (
        session_id, agent_id, agent_type, workspace, status, progress, step_current,
        step_total, message, snippet, metadata, updated_at, original_status, stuck_at, git_branch
    )
    VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, NULL, ?)
    ON CONFLICT(session_id, agent_id) DO UPDATE SET
        agent_type = excluded.agent_type,
        workspace = excluded.workspace,
        status = excluded.status,
        progress = excluded.progress,
        step_current = excluded.step_current,
        step_total = excluded.step_total,
        message = excluded.message,
        snippet = excluded.snippet,
        metadata = excluded.metadata,
        updated_at = excluded.updated_at,
        original_status = NULL,
        stuck_at = NULL,
        git_branch = excluded.git_branch
`, report.SessionID, report.AgentID, report.AgentType, report.Workspace, string(report.Status),
    nullableInt(report.Progress), nullableInt(report.StepCurrent), nullableInt(report.StepTotal),
    report.Message, report.Snippet, string(metadata), now, nullableString(report.GitBranch)); err != nil {
    return fmt.Errorf("upserting agent state: %w", err)
}
```

Update `listAgentStates` query to select `git_branch`:

```go
rows, err := s.db.QueryContext(ctx, `
    SELECT session_id, agent_id, agent_type, workspace, status, progress, step_current,
        step_total, message, snippet, metadata, updated_at, original_status, stuck_at, git_branch
    FROM agent_states
    WHERE session_id = ?
    ORDER BY updated_at DESC
`, sessionID)
```

Update `scanAgentState` to scan `git_branch`:

```go
func scanAgentState(row rowScanner) (AgentState, error) {
	var state AgentState
	var progress, stepCurrent, stepTotal sql.NullInt64
	var metadata, updatedAt string
	var originalStatus, stuckAt, gitBranch sql.NullString
	if err := row.Scan(
		&state.SessionID, &state.AgentID, &state.AgentType, &state.Workspace,
		&state.Status, &progress, &stepCurrent, &stepTotal, &state.Message, &state.Snippet,
		&metadata, &updatedAt, &originalStatus, &stuckAt, &gitBranch,
	); err != nil {
		return state, fmt.Errorf("scanning agent state: %w", err)
	}
	state.Progress = intPtr(progress)
	state.StepCurrent = intPtr(stepCurrent)
	state.StepTotal = intPtr(stepTotal)
	state.Metadata = parseMetadata(metadata)
	state.UpdatedAt = parseTime(updatedAt)
	if originalStatus.Valid {
		s := Status(originalStatus.String)
		state.OriginalStatus = &s
	}
	if stuckAt.Valid {
		t := parseTime(stuckAt.String)
		state.StuckAt = &t
	}
	if gitBranch.Valid {
		state.GitBranch = gitBranch.String
	}
	return state, nil
}
```

- [ ] **Step 5: Fix `RecordReport` callers — update `Service.Report` in service.go**

In `internal/status/service.go`, the call to `s.store.RecordReport` passes an `Event` and session title. The `Store` interface method is:
```go
RecordReport(ctx context.Context, report Event, sessionTitle string) error
```

The `Event` struct does not carry `GitBranch` — the branch is on `ReportInput`. Update `Service.Report` to pass `GitBranch` from `normalized` input to the event. Since `Event` doesn't have GitBranch, pass it via the `ReportInput.GitBranch` field directly in the storage call. 

Actually the simplest approach: store `GitBranch` in the event by adding it to `Event` struct in `models.go` as well:

Add `GitBranch string \`json:"git_branch,omitempty"\`` to the `Event` struct in `internal/status/models.go`:

```go
type Event struct {
	ID          string         `json:"id"`
	SessionID   string         `json:"session_id"`
	AgentID     string         `json:"agent_id"`
	AgentType   string         `json:"agent_type"`
	Workspace   string         `json:"workspace"`
	Status      Status         `json:"status"`
	Progress    *int           `json:"progress,omitempty"`
	StepCurrent *int           `json:"step_current,omitempty"`
	StepTotal   *int           `json:"step_total,omitempty"`
	Message     string         `json:"message"`
	Snippet     string         `json:"snippet"`
	Metadata    map[string]any `json:"metadata"`
	CreatedAt   time.Time      `json:"created_at"`
	GitBranch   string         `json:"git_branch,omitempty"`
}
```

In `internal/status/service.go`, in `Service.Report`, add `GitBranch` when building the event:

```go
event := Event{
    ID:          eventID,
    SessionID:   normalized.SessionID,
    AgentID:     normalized.AgentID,
    AgentType:   normalized.AgentType,
    Workspace:   normalized.Workspace,
    Status:      normalized.Status,
    Progress:    normalized.Progress,
    StepCurrent: normalized.StepCurrent,
    StepTotal:   normalized.StepTotal,
    Message:     normalized.Message,
    Snippet:     normalized.Snippet,
    Metadata:    normalized.Metadata,
    CreatedAt:   now,
    GitBranch:   normalized.GitBranch,
}
```

In `internal/status/storage.go`, in `RecordReport`, pass `report.GitBranch` as `nullableString(report.GitBranch)` (already done in step 4).

- [ ] **Step 6: Run tests**

```bash
go test ./internal/status/... -v
```

Expected: All tests pass including `TestStorageRecordReportStoresGitBranch`.

- [ ] **Step 7: Update MCP and CLI callers to pass git_branch**

In `cmd/mcp/report_status_tool.go`, add `git_branch` parameter and field to input:

Add to the tool parameters (after `"metadata"`):
```go
mcplib.String("git_branch", "Optional current git branch name"),
```

Add to the input struct:
```go
input.GitBranch = req.StringOr("git_branch", "")
```

In `cmd/report.go`, add the `--git-branch` flag and populate `input.GitBranch`:

Add to Flags:
```go
&cli.StringFlag{Name: "git-branch", Usage: "Optional current git branch"},
```

Add in `reportInputFromCommand`:
```go
input.GitBranch = cmd.GetString("git-branch")
```

- [ ] **Step 8: Run full test suite**

```bash
go test ./... -count=1
```

Expected: all tests pass.

- [ ] **Step 9: Commit**

```bash
git add internal/status/models.go internal/status/storage.go internal/status/storage_test.go \
    internal/status/service.go cmd/mcp/report_status_tool.go cmd/report.go
git commit -m "feat(status): add git_branch field to ReportInput, AgentState, and Event"
```

---

## Task 3: Blackboard models

**Files:**
- Create: `internal/blackboard/models.go`

- [ ] **Step 1: Write the failing test** — create `internal/blackboard/models_test.go` (compile guard)

```go
package blackboard

import "testing"

func TestModelConstants(t *testing.T) {
	_ = ScopeSession
	_ = ScopeBranch
	_ = ScopeProject
	_ = TypeFinding
	_ = TypeDecision
	_ = TypeBug
	_ = TypeDebt
	_ = TypeWarning
	_ = TypeContext
	_ = ErrInvalidInput
	_ = ErrNotFound
	_ = ErrAlreadyAtTopScope
	var _ Entry
	var _ WriteInput
	var _ WriteResult
	var _ Bundle
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
go test ./internal/blackboard/... -run TestModelConstants -v
```

Expected: FAIL — package doesn't exist yet.

- [ ] **Step 3: Create `internal/blackboard/models.go`**

```go
package blackboard

import (
	"errors"
	"time"
)

type Scope string
type EntryType string

const (
	ScopeSession Scope = "session"
	ScopeBranch  Scope = "branch"
	ScopeProject Scope = "project"
)

const (
	TypeFinding  EntryType = "finding"
	TypeDecision EntryType = "decision"
	TypeBug      EntryType = "bug"
	TypeDebt     EntryType = "debt"
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
	ErrInvalidInput      = errors.New("invalid blackboard input")
	ErrNotFound          = errors.New("not found")
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

type WriteResult struct {
	ID    string `json:"id"`
	Scope Scope  `json:"scope"`
}

type Bundle struct {
	Entries        []Entry `json:"entries"`
	MarkdownBundle string  `json:"markdown_bundle"`
}
```

- [ ] **Step 4: Run test to verify it passes**

```bash
go test ./internal/blackboard/... -run TestModelConstants -v
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/blackboard/models.go internal/blackboard/models_test.go
git commit -m "feat(blackboard): add models"
```

---

## Task 4: Blackboard storage

**Files:**
- Create: `internal/blackboard/storage.go`
- Create: `internal/blackboard/storage_test.go`

- [ ] **Step 1: Write the failing tests** in `internal/blackboard/storage_test.go`

```go
package blackboard

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/martinsuchenak/skopos/internal/db"
	_ "modernc.org/sqlite"
)

func testStorage(t *testing.T) *Storage {
	t.Helper()
	sqlDB, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	if err := db.RunMigrations(sqlDB); err != nil {
		t.Fatalf("run migrations: %v", err)
	}
	return NewStorage(sqlDB)
}

func TestStorageWriteAndBundle(t *testing.T) {
	s := testStorage(t)
	ctx := context.Background()
	now := time.Now().UTC()

	entry := Entry{
		ID:            "entry-1",
		Scope:         ScopeBranch,
		BranchName:    "feat-auth",
		EntryType:     TypeFinding,
		Title:         "JWT not checked",
		Content:       "Tokens bypass expiry.",
		CodeRef:       "auth/jwt.go:45",
		AuthorAgentID: "agent-1",
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := s.Write(ctx, entry); err != nil {
		t.Fatalf("write: %v", err)
	}

	entries, err := s.Bundle(ctx, "feat-auth", "")
	if err != nil {
		t.Fatalf("bundle: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].Title != "JWT not checked" {
		t.Errorf("title: got %q", entries[0].Title)
	}
	if entries[0].CodeRef != "auth/jwt.go:45" {
		t.Errorf("code_ref: got %q", entries[0].CodeRef)
	}
}

func TestStorageBundleFloatingTypesAlwaysIncluded(t *testing.T) {
	s := testStorage(t)
	ctx := context.Background()
	now := time.Now().UTC()

	// Bug on feat-auth branch
	if err := s.Write(ctx, Entry{
		ID: "bug-1", Scope: ScopeBranch, BranchName: "feat-auth",
		EntryType: TypeBug, Title: "Critical bug", AuthorAgentID: "a",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("write bug: %v", err)
	}

	// Query from a different branch — bug should still appear
	entries, err := s.Bundle(ctx, "main", "")
	if err != nil {
		t.Fatalf("bundle: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 floating entry, got %d", len(entries))
	}
	if entries[0].EntryType != TypeBug {
		t.Errorf("expected bug type, got %q", entries[0].EntryType)
	}
}

func TestStorageBundleSessionScope(t *testing.T) {
	s := testStorage(t)
	ctx := context.Background()
	now := time.Now().UTC()

	// Create a sessions row so the FK is satisfied
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions (id, title, workspace, status, started_at, updated_at)
		 VALUES ('sess-1', 'test', '/repo', 'running', ?, ?)`,
		formatTime(now), formatTime(now))
	if err != nil {
		t.Fatalf("insert session: %v", err)
	}

	if err := s.Write(ctx, Entry{
		ID: "e-1", Scope: ScopeSession, SessionID: "sess-1",
		EntryType: TypeContext, Title: "Local context",
		AuthorAgentID: "a", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Session entry visible with matching session_id
	entries, err := s.Bundle(ctx, "", "sess-1")
	if err != nil {
		t.Fatalf("bundle: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry with session, got %d", len(entries))
	}

	// Not visible for a different session
	entries, err = s.Bundle(ctx, "", "sess-other")
	if err != nil {
		t.Fatalf("bundle other: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected 0 entries for other session, got %d", len(entries))
	}
}

func TestStorageOnDeleteCascadeRemovesSessionEntries(t *testing.T) {
	s := testStorage(t)
	ctx := context.Background()
	now := time.Now().UTC()

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions (id, title, workspace, status, started_at, updated_at)
		 VALUES ('sess-del', 'test', '/repo', 'running', ?, ?)`, formatTime(now), formatTime(now))
	if err != nil {
		t.Fatalf("insert session: %v", err)
	}
	if err := s.Write(ctx, Entry{
		ID: "e-del", Scope: ScopeSession, SessionID: "sess-del",
		EntryType: TypeContext, Title: "Gone soon",
		AuthorAgentID: "a", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, err = s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id = 'sess-del'`)
	if err != nil {
		t.Fatalf("delete session: %v", err)
	}

	entries, err := s.Bundle(ctx, "", "sess-del")
	if err != nil {
		t.Fatalf("bundle: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected 0 entries after cascade delete, got %d", len(entries))
	}
}

func TestStoragePromote(t *testing.T) {
	s := testStorage(t)
	ctx := context.Background()
	now := time.Now().UTC()

	// session -> branch requires a sessions row
	_, _ = s.db.ExecContext(ctx,
		`INSERT INTO sessions (id, title, workspace, status, started_at, updated_at) VALUES ('s1','t','/r','running',?,?)`,
		formatTime(now), formatTime(now))

	if err := s.Write(ctx, Entry{
		ID: "p-1", Scope: ScopeSession, SessionID: "s1",
		BranchName: "feat", EntryType: TypeFinding, Title: "Promote me",
		AuthorAgentID: "a", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := s.Promote(ctx, "p-1"); err != nil {
		t.Fatalf("promote session->branch: %v", err)
	}
	e, err := s.Get(ctx, "p-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if e.Scope != ScopeBranch {
		t.Errorf("expected branch scope, got %q", e.Scope)
	}

	if err := s.Promote(ctx, "p-1"); err != nil {
		t.Fatalf("promote branch->project: %v", err)
	}
	e, err = s.Get(ctx, "p-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if e.Scope != ScopeProject {
		t.Errorf("expected project scope, got %q", e.Scope)
	}
	if e.BranchName != "" {
		t.Errorf("branch_name should be empty at project scope, got %q", e.BranchName)
	}

	err = s.Promote(ctx, "p-1")
	if err == nil {
		t.Fatal("expected error promoting project-scope entry")
	}
	if !isErrAlreadyAtTopScope(err) {
		t.Errorf("expected ErrAlreadyAtTopScope, got %v", err)
	}
}

func TestStorageDelete(t *testing.T) {
	s := testStorage(t)
	ctx := context.Background()
	now := time.Now().UTC()

	if err := s.Write(ctx, Entry{
		ID: "del-1", Scope: ScopeProject, EntryType: TypeDecision,
		Title: "To delete", AuthorAgentID: "a", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := s.Delete(ctx, "del-1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	_, err := s.Get(ctx, "del-1")
	if err == nil {
		t.Fatal("expected ErrNotFound after delete")
	}
}

func isErrAlreadyAtTopScope(err error) bool {
	return errors.Is(err, ErrAlreadyAtTopScope)
}
```

- [ ] **Step 2: Run tests to verify they fail**

```bash
go test ./internal/blackboard/... -v
```

Expected: FAIL — `Storage` type not defined.

- [ ] **Step 3: Create `internal/blackboard/storage.go`**

```go
package blackboard

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

type Store interface {
	Write(ctx context.Context, entry Entry) error
	Bundle(ctx context.Context, branchName, sessionID string) ([]Entry, error)
	Promote(ctx context.Context, id string) error
	Delete(ctx context.Context, id string) error
	Get(ctx context.Context, id string) (*Entry, error)
}

type Storage struct {
	db *sql.DB
}

func NewStorage(db *sql.DB) *Storage {
	return &Storage{db: db}
}

func (s *Storage) Write(ctx context.Context, entry Entry) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO blackboard_entries (
			id, scope, branch_name, session_id, entry_type, title, content, code_ref,
			author_agent_id, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, entry.ID,
		string(entry.Scope),
		nullableString(entry.BranchName),
		nullableString(entry.SessionID),
		string(entry.EntryType),
		entry.Title,
		entry.Content,
		nullableString(entry.CodeRef),
		entry.AuthorAgentID,
		formatTime(entry.CreatedAt),
		formatTime(entry.UpdatedAt),
	)
	if err != nil {
		return fmt.Errorf("inserting blackboard entry: %w", err)
	}
	return nil
}

func (s *Storage) Bundle(ctx context.Context, branchName, sessionID string) ([]Entry, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, scope, branch_name, session_id, entry_type, title, content, code_ref,
		       author_agent_id, created_at, updated_at
		FROM blackboard_entries
		WHERE scope = 'project'
		   OR (scope = 'branch' AND branch_name = ?)
		   OR (scope = 'session' AND session_id = ?)
		   OR entry_type IN ('bug', 'debt')
		ORDER BY entry_type, created_at ASC
	`, branchName, sessionID)
	if err != nil {
		return nil, fmt.Errorf("querying bundle: %w", err)
	}
	defer rows.Close()

	var entries []Entry
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating entries: %w", err)
	}
	return entries, nil
}

func (s *Storage) Get(ctx context.Context, id string) (*Entry, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, scope, branch_name, session_id, entry_type, title, content, code_ref,
		       author_agent_id, created_at, updated_at
		FROM blackboard_entries WHERE id = ?
	`, id)
	e, err := scanEntry(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: entry %s", ErrNotFound, id)
		}
		return nil, err
	}
	return &e, nil
}

func (s *Storage) Promote(ctx context.Context, id string) error {
	entry, err := s.Get(ctx, id)
	if err != nil {
		return err
	}

	var newScope Scope
	var newBranch any
	switch entry.Scope {
	case ScopeSession:
		newScope = ScopeBranch
		newBranch = nullableString(entry.BranchName)
	case ScopeBranch:
		newScope = ScopeProject
		newBranch = nil
	case ScopeProject:
		return ErrAlreadyAtTopScope
	}

	now := formatTime(time.Now().UTC())
	_, err = s.db.ExecContext(ctx, `
		UPDATE blackboard_entries
		SET scope = ?, branch_name = ?, session_id = NULL, updated_at = ?
		WHERE id = ?
	`, string(newScope), newBranch, now, id)
	if err != nil {
		return fmt.Errorf("promoting entry: %w", err)
	}
	return nil
}

func (s *Storage) Delete(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM blackboard_entries WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("deleting entry: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("checking delete result: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("%w: entry %s", ErrNotFound, id)
	}
	return nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanEntry(row rowScanner) (Entry, error) {
	var e Entry
	var branchName, sessionID, codeRef sql.NullString
	var createdAt, updatedAt string
	if err := row.Scan(
		&e.ID, &e.Scope, &branchName, &sessionID, &e.EntryType,
		&e.Title, &e.Content, &codeRef, &e.AuthorAgentID, &createdAt, &updatedAt,
	); err != nil {
		return e, fmt.Errorf("scanning entry: %w", err)
	}
	if branchName.Valid {
		e.BranchName = branchName.String
	}
	if sessionID.Valid {
		e.SessionID = sessionID.String
	}
	if codeRef.Valid {
		e.CodeRef = codeRef.String
	}
	e.CreatedAt = parseTime(createdAt)
	e.UpdatedAt = parseTime(updatedAt)
	return e, nil
}

func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func formatTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

func parseTime(raw string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}
	}
	return t
}
```

Also add the missing import to the test file — add `"errors"` to the import block in `storage_test.go`.

- [ ] **Step 4: Run tests to verify they pass**

```bash
go test ./internal/blackboard/... -run "TestStorage" -v
```

Expected: All storage tests PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/blackboard/storage.go internal/blackboard/storage_test.go
git commit -m "feat(blackboard): storage layer with full test coverage"
```

---

## Task 5: Blackboard service

**Files:**
- Create: `internal/blackboard/service.go`
- Create: `internal/blackboard/service_test.go`

- [ ] **Step 1: Write the failing tests** in `internal/blackboard/service_test.go`

```go
package blackboard

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type fakeStore struct {
	entries  []Entry
	writeErr error
}

func (f *fakeStore) Write(_ context.Context, e Entry) error {
	if f.writeErr != nil {
		return f.writeErr
	}
	f.entries = append(f.entries, e)
	return nil
}
func (f *fakeStore) Bundle(_ context.Context, _, _ string) ([]Entry, error) {
	return f.entries, nil
}
func (f *fakeStore) Promote(_ context.Context, _ string) error { return nil }
func (f *fakeStore) Delete(_ context.Context, _ string) error  { return nil }
func (f *fakeStore) Get(_ context.Context, _ string) (*Entry, error) {
	return nil, ErrNotFound
}

func TestServiceWriteValidationMissingTitle(t *testing.T) {
	svc := NewService(&fakeStore{})
	_, err := svc.Write(context.Background(), WriteInput{
		Scope: ScopeProject, EntryType: TypeFinding, AuthorAgentID: "agent-1",
	})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput, got %v", err)
	}
}

func TestServiceWriteValidationMissingAuthorAgentID(t *testing.T) {
	svc := NewService(&fakeStore{})
	_, err := svc.Write(context.Background(), WriteInput{
		Scope: ScopeProject, EntryType: TypeFinding, Title: "T",
	})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput, got %v", err)
	}
}

func TestServiceWriteValidationInvalidScope(t *testing.T) {
	svc := NewService(&fakeStore{})
	_, err := svc.Write(context.Background(), WriteInput{
		Scope: "global", EntryType: TypeFinding, Title: "T", AuthorAgentID: "a",
	})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput, got %v", err)
	}
}

func TestServiceWriteValidationInvalidEntryType(t *testing.T) {
	svc := NewService(&fakeStore{})
	_, err := svc.Write(context.Background(), WriteInput{
		Scope: ScopeProject, EntryType: "memo", Title: "T", AuthorAgentID: "a",
	})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput, got %v", err)
	}
}

func TestServiceWriteValidationBranchScopeRequiresBranchName(t *testing.T) {
	svc := NewService(&fakeStore{})
	_, err := svc.Write(context.Background(), WriteInput{
		Scope: ScopeBranch, EntryType: TypeFinding, Title: "T", AuthorAgentID: "a",
	})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput, got %v", err)
	}
}

func TestServiceWriteValidationSessionScopeRequiresSessionID(t *testing.T) {
	svc := NewService(&fakeStore{})
	_, err := svc.Write(context.Background(), WriteInput{
		Scope: ScopeSession, EntryType: TypeContext, Title: "T", AuthorAgentID: "a",
	})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput, got %v", err)
	}
}

func TestServiceWriteSuccess(t *testing.T) {
	store := &fakeStore{}
	svc := NewService(store)
	result, err := svc.Write(context.Background(), WriteInput{
		Scope:         ScopeProject,
		EntryType:     TypeFinding,
		Title:         "Found something",
		Content:       "Details here.",
		AuthorAgentID: "agent-1",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.ID == "" {
		t.Fatal("expected generated ID")
	}
	if result.Scope != ScopeProject {
		t.Errorf("scope: got %q, want %q", result.Scope, ScopeProject)
	}
	if len(store.entries) != 1 {
		t.Fatalf("expected 1 stored entry, got %d", len(store.entries))
	}
}

func TestServiceBundleMarkdownNotEmpty(t *testing.T) {
	store := &fakeStore{entries: []Entry{
		{EntryType: TypeBug, Title: "Crash on nil", AuthorAgentID: "a", Scope: ScopeBranch, BranchName: "feat"},
	}}
	svc := NewService(store)
	bundle, err := svc.Bundle(context.Background(), "feat", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if bundle.MarkdownBundle == "" {
		t.Fatal("expected non-empty MarkdownBundle")
	}
	if !strings.Contains(bundle.MarkdownBundle, "Crash on nil") {
		t.Errorf("expected entry title in markdown, got: %s", bundle.MarkdownBundle)
	}
}

func TestServiceBundleEmptyMarkdown(t *testing.T) {
	svc := NewService(&fakeStore{})
	bundle, err := svc.Bundle(context.Background(), "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if bundle.MarkdownBundle == "" {
		t.Fatal("expected non-empty markdown even with no entries")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

```bash
go test ./internal/blackboard/... -run "TestService" -v
```

Expected: FAIL — `Service` type not defined.

- [ ] **Step 3: Create `internal/blackboard/service.go`**

```go
package blackboard

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

type Service struct {
	store Store
	now   func() time.Time
}

func NewService(store Store) *Service {
	return &Service{store: store, now: time.Now}
}

func (s *Service) Write(ctx context.Context, input WriteInput) (*WriteResult, error) {
	input.Scope = Scope(strings.TrimSpace(string(input.Scope)))
	input.EntryType = EntryType(strings.TrimSpace(string(input.EntryType)))
	input.Title = strings.TrimSpace(input.Title)
	input.AuthorAgentID = strings.TrimSpace(input.AuthorAgentID)
	input.BranchName = strings.TrimSpace(input.BranchName)
	input.SessionID = strings.TrimSpace(input.SessionID)

	if input.Title == "" {
		return nil, fmt.Errorf("%w: title is required", ErrInvalidInput)
	}
	if input.AuthorAgentID == "" {
		return nil, fmt.Errorf("%w: author_agent_id is required", ErrInvalidInput)
	}
	if !validScope(input.Scope) {
		return nil, fmt.Errorf("%w: invalid scope %q", ErrInvalidInput, input.Scope)
	}
	if !validEntryType(input.EntryType) {
		return nil, fmt.Errorf("%w: invalid entry_type %q", ErrInvalidInput, input.EntryType)
	}
	if input.Scope == ScopeBranch && input.BranchName == "" {
		return nil, fmt.Errorf("%w: branch_name is required for branch scope", ErrInvalidInput)
	}
	if input.Scope == ScopeSession && input.SessionID == "" {
		return nil, fmt.Errorf("%w: session_id is required for session scope", ErrInvalidInput)
	}

	now := s.now().UTC()
	entry := Entry{
		ID:            generateID(),
		Scope:         input.Scope,
		BranchName:    input.BranchName,
		SessionID:     input.SessionID,
		EntryType:     input.EntryType,
		Title:         input.Title,
		Content:       strings.TrimSpace(input.Content),
		CodeRef:       strings.TrimSpace(input.CodeRef),
		AuthorAgentID: input.AuthorAgentID,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := s.store.Write(ctx, entry); err != nil {
		return nil, err
	}
	return &WriteResult{ID: entry.ID, Scope: entry.Scope}, nil
}

func (s *Service) Bundle(ctx context.Context, branchName, sessionID string) (*Bundle, error) {
	entries, err := s.store.Bundle(ctx, strings.TrimSpace(branchName), strings.TrimSpace(sessionID))
	if err != nil {
		return nil, err
	}
	return &Bundle{
		Entries:        entries,
		MarkdownBundle: formatMarkdown(branchName, entries),
	}, nil
}

func (s *Service) Promote(ctx context.Context, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("%w: id is required", ErrInvalidInput)
	}
	return s.store.Promote(ctx, id)
}

func (s *Service) Delete(ctx context.Context, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("%w: id is required", ErrInvalidInput)
	}
	return s.store.Delete(ctx, id)
}

func validScope(s Scope) bool {
	switch s {
	case ScopeSession, ScopeBranch, ScopeProject:
		return true
	}
	return false
}

func validEntryType(t EntryType) bool {
	switch t {
	case TypeFinding, TypeDecision, TypeBug, TypeDebt, TypeWarning, TypeContext:
		return true
	}
	return false
}

func formatMarkdown(branchName string, entries []Entry) string {
	var sb strings.Builder
	sb.WriteString("## Skopos Knowledge Bundle\n")
	if branchName != "" {
		sb.WriteString(fmt.Sprintf("### Branch: %s\n\n", branchName))
	} else {
		sb.WriteString("\n")
	}

	if len(entries) == 0 {
		sb.WriteString("_No entries found._\n")
		return sb.String()
	}

	byType := make(map[EntryType][]Entry)
	for _, e := range entries {
		byType[e.EntryType] = append(byType[e.EntryType], e)
	}

	order := []EntryType{TypeBug, TypeDebt, TypeWarning, TypeFinding, TypeDecision, TypeContext}
	labels := map[EntryType]string{
		TypeBug:      "🐛 Bugs (cross-branch)",
		TypeDebt:     "⚠️ Tech Debt (cross-branch)",
		TypeWarning:  "⚠️ Warnings",
		TypeFinding:  "🔍 Findings",
		TypeDecision: "✅ Decisions",
		TypeContext:  "📋 Context",
	}

	for _, t := range order {
		es, ok := byType[t]
		if !ok {
			continue
		}
		sb.WriteString(fmt.Sprintf("#### %s\n", labels[t]))
		for _, e := range es {
			ref := ""
			if e.CodeRef != "" {
				ref = fmt.Sprintf(" (%s)", e.CodeRef)
			}
			sb.WriteString(fmt.Sprintf("- **%s**%s\n", e.Title, ref))
			if e.Content != "" {
				sb.WriteString(fmt.Sprintf("  %s\n", e.Content))
			}
			sb.WriteString(fmt.Sprintf("  _— %s_\n", e.AuthorAgentID))
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

func generateID() string {
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.NewString()
	}
	return id.String()
}
```

- [ ] **Step 4: Run tests to verify they pass**

```bash
go test ./internal/blackboard/... -v
```

Expected: All tests PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/blackboard/service.go internal/blackboard/service_test.go
git commit -m "feat(blackboard): service layer with validation and markdown bundle"
```

---

## Task 6: Blackboard handler

**Files:**
- Create: `internal/blackboard/handler.go`
- Create: `internal/blackboard/handler_test.go`

- [ ] **Step 1: Write the failing tests** in `internal/blackboard/handler_test.go`

```go
package blackboard

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/martinsuchenak/skopos/internal/db"
	_ "modernc.org/sqlite"
)

func testHandler(t *testing.T, apiKey string) *Handler {
	t.Helper()
	sqlDB, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	if err := db.RunMigrations(sqlDB); err != nil {
		t.Fatalf("run migrations: %v", err)
	}
	return NewHandler(NewService(NewStorage(sqlDB)), apiKey)
}

func TestHandlerWriteRequiresAPIKey(t *testing.T) {
	h := testHandler(t, "secret")
	body := bytes.NewBufferString(`{
		"scope":"project","entry_type":"finding","title":"T","author_agent_id":"a"
	}`)
	req := httptest.NewRequest("POST", "/api/blackboard/entries", body)
	w := httptest.NewRecorder()
	h.WriteEntry(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestHandlerWriteAndReadBundle(t *testing.T) {
	h := testHandler(t, "")
	body := bytes.NewBufferString(`{
		"scope":"project","entry_type":"finding","title":"Auth issue",
		"content":"Details.","author_agent_id":"agent-1"
	}`)
	req := httptest.NewRequest("POST", "/api/blackboard/entries", body)
	w := httptest.NewRecorder()
	h.WriteEntry(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d body=%s", w.Code, w.Body.String())
	}
	var result WriteResult
	if err := json.NewDecoder(w.Body).Decode(&result); err != nil {
		t.Fatalf("decode write result: %v", err)
	}
	if result.ID == "" {
		t.Fatal("expected ID in result")
	}

	req2 := httptest.NewRequest("GET", "/api/blackboard/entries", nil)
	w2 := httptest.NewRecorder()
	h.ReadBundle(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w2.Code)
	}
	var bundle Bundle
	if err := json.NewDecoder(w2.Body).Decode(&bundle); err != nil {
		t.Fatalf("decode bundle: %v", err)
	}
	if len(bundle.Entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(bundle.Entries))
	}
	if bundle.MarkdownBundle == "" {
		t.Fatal("expected non-empty MarkdownBundle")
	}
}

func TestHandlerWriteRejectsInvalidPayload(t *testing.T) {
	h := testHandler(t, "")
	body := bytes.NewBufferString(`{"scope":"project","entry_type":"finding"}`)
	req := httptest.NewRequest("POST", "/api/blackboard/entries", body)
	w := httptest.NewRecorder()
	h.WriteEntry(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestHandlerPromoteNotFound(t *testing.T) {
	h := testHandler(t, "")
	req := httptest.NewRequest("PATCH", "/api/blackboard/entries/missing/promote", nil)
	req.SetPathValue("id", "missing")
	w := httptest.NewRecorder()
	h.Promote(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestHandlerDeleteNotFound(t *testing.T) {
	h := testHandler(t, "")
	req := httptest.NewRequest("DELETE", "/api/blackboard/entries/missing", nil)
	req.SetPathValue("id", "missing")
	w := httptest.NewRecorder()
	h.Delete(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

```bash
go test ./internal/blackboard/... -run "TestHandler" -v
```

Expected: FAIL — `Handler` not defined.

- [ ] **Step 3: Create `internal/blackboard/handler.go`**

```go
package blackboard

import (
	"errors"
	"net/http"

	"github.com/martinsuchenak/skopos/internal/rest"
)

type Handler struct {
	service *Service
	apiKey  string
}

func NewHandler(service *Service, apiKey string) *Handler {
	return &Handler{service: service, apiKey: apiKey}
}

func (h *Handler) WriteEntry(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var input WriteInput
	if err := rest.DecodeJSON(r, &input); err != nil {
		rest.RespondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	result, err := h.service.Write(r.Context(), input)
	if err != nil {
		if errors.Is(err, ErrInvalidInput) {
			rest.RespondError(w, http.StatusBadRequest, err.Error())
			return
		}
		rest.RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	rest.RespondJSON(w, http.StatusCreated, result)
}

func (h *Handler) ReadBundle(w http.ResponseWriter, r *http.Request) {
	branch := r.URL.Query().Get("branch")
	sessionID := r.URL.Query().Get("session_id")
	bundle, err := h.service.Bundle(r.Context(), branch, sessionID)
	if err != nil {
		rest.RespondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	rest.RespondJSON(w, http.StatusOK, bundle)
}

func (h *Handler) Promote(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if err := h.service.Promote(r.Context(), r.PathValue("id")); err != nil {
		switch {
		case errors.Is(err, ErrNotFound):
			rest.RespondError(w, http.StatusNotFound, err.Error())
		case errors.Is(err, ErrAlreadyAtTopScope):
			rest.RespondError(w, http.StatusConflict, err.Error())
		case errors.Is(err, ErrInvalidInput):
			rest.RespondError(w, http.StatusBadRequest, err.Error())
		default:
			rest.RespondError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		rest.RespondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if err := h.service.Delete(r.Context(), r.PathValue("id")); err != nil {
		switch {
		case errors.Is(err, ErrNotFound):
			rest.RespondError(w, http.StatusNotFound, err.Error())
		case errors.Is(err, ErrInvalidInput):
			rest.RespondError(w, http.StatusBadRequest, err.Error())
		default:
			rest.RespondError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) authorized(r *http.Request) bool {
	if h.apiKey == "" {
		return true
	}
	key := r.Header.Get("X-API-Key")
	if key == "" {
		key = r.URL.Query().Get("api_key")
	}
	return key == h.apiKey
}
```

- [ ] **Step 4: Run all blackboard tests**

```bash
go test ./internal/blackboard/... -v
```

Expected: All tests PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/blackboard/handler.go internal/blackboard/handler_test.go
git commit -m "feat(blackboard): REST handler"
```

---

## Task 7: Route registration

**Files:**
- Modify: `cmd/routes/api_routes.go`
- Create: `cmd/routes/blackboard_routes.go`
- Create: `cmd/routes/blackboard_routes_test.go`

- [ ] **Step 1: Write the failing test** in `cmd/routes/blackboard_routes_test.go`

```go
package routes

import (
	"net/http"
	"testing"

	"github.com/martinsuchenak/skopos/internal/blackboard"
)

func TestRegisterBlackboardRoutes(t *testing.T) {
	mux := http.NewServeMux()
	registerBlackboardRoutes(mux, blackboard.NewHandler(
		blackboard.NewService(&noopBlackboardStore{}), "",
	))
}

type noopBlackboardStore struct{}

func (s *noopBlackboardStore) Write(_ interface{}, _ blackboard.Entry) error { return nil }
func (s *noopBlackboardStore) Bundle(_ interface{}, _, _ string) ([]blackboard.Entry, error) {
	return nil, nil
}
func (s *noopBlackboardStore) Promote(_ interface{}, _ string) error { return nil }
func (s *noopBlackboardStore) Delete(_ interface{}, _ string) error  { return nil }
func (s *noopBlackboardStore) Get(_ interface{}, _ string) (*blackboard.Entry, error) {
	return nil, blackboard.ErrNotFound
}
```

Note: The `noopBlackboardStore` uses `interface{}` for ctx parameters as a placeholder — replace with `context.Context` and add `"context"` import. Full correct version:

```go
package routes

import (
	"context"
	"net/http"
	"testing"

	"github.com/martinsuchenak/skopos/internal/blackboard"
)

func TestRegisterBlackboardRoutes(t *testing.T) {
	mux := http.NewServeMux()
	registerBlackboardRoutes(mux, blackboard.NewHandler(
		blackboard.NewService(&noopBlackboardStore{}), "",
	))
}

type noopBlackboardStore struct{}

func (s *noopBlackboardStore) Write(_ context.Context, _ blackboard.Entry) error { return nil }
func (s *noopBlackboardStore) Bundle(_ context.Context, _, _ string) ([]blackboard.Entry, error) {
	return nil, nil
}
func (s *noopBlackboardStore) Promote(_ context.Context, _ string) error { return nil }
func (s *noopBlackboardStore) Delete(_ context.Context, _ string) error  { return nil }
func (s *noopBlackboardStore) Get(_ context.Context, _ string) (*blackboard.Entry, error) {
	return nil, blackboard.ErrNotFound
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
go test ./cmd/routes/... -run TestRegisterBlackboardRoutes -v
```

Expected: FAIL — `registerBlackboardRoutes` not defined.

- [ ] **Step 3: Update `cmd/routes/api_routes.go`**

Replace the entire file contents with (adding `blackboardRegistrations`, `RegisterBlackboard`, and updating `RegisterRoutes` signature):

```go
package routes

import (
	"encoding/json"
	"html/template"
	"io/fs"
	"net/http"
	"runtime"

	"github.com/martinsuchenak/skopos/internal/blackboard"
	"github.com/martinsuchenak/skopos/internal/status"
	appweb "github.com/martinsuchenak/skopos/web"
)

var registrations []func(*http.ServeMux, *status.Handler)
var blackboardRegistrations []func(*http.ServeMux, *blackboard.Handler)

func Register(fn func(*http.ServeMux, *status.Handler)) {
	registrations = append(registrations, fn)
}

func RegisterBlackboard(fn func(*http.ServeMux, *blackboard.Handler)) {
	blackboardRegistrations = append(blackboardRegistrations, fn)
}

func RegisterRoutes(mux *http.ServeMux, statusHandler *status.Handler, blackboardHandler *blackboard.Handler) {
	mux.HandleFunc("GET /health", healthHandler)
	mux.HandleFunc("GET /metrics", metricsHandler)
	registerWebRoutes(mux)

	for _, fn := range registrations {
		fn(mux, statusHandler)
	}
	for _, fn := range blackboardRegistrations {
		fn(mux, blackboardHandler)
	}
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func metricsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"goroutines": runtime.NumGoroutine(),
		"alloc_mb":   m.Alloc / 1024 / 1024,
	})
}

func registerWebRoutes(mux *http.ServeMux) {
	templates := template.Must(template.ParseFS(appweb.TemplateFiles, "templates/base.html"))
	staticFS, err := fs.Sub(appweb.StaticFiles, "dist")
	if err == nil {
		mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticFS))))
	}
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		templates.ExecuteTemplate(w, "base.html", map[string]any{"Title": "Dashboard"})
	})
}
```

- [ ] **Step 4: Create `cmd/routes/blackboard_routes.go`**

```go
package routes

import (
	"net/http"

	"github.com/martinsuchenak/skopos/internal/blackboard"
)

func init() {
	RegisterBlackboard(registerBlackboardRoutes)
}

func registerBlackboardRoutes(mux *http.ServeMux, h *blackboard.Handler) {
	mux.HandleFunc("POST /api/blackboard/entries", h.WriteEntry)
	mux.HandleFunc("GET /api/blackboard/entries", h.ReadBundle)
	mux.HandleFunc("PATCH /api/blackboard/entries/{id}/promote", h.Promote)
	mux.HandleFunc("DELETE /api/blackboard/entries/{id}", h.Delete)
}
```

- [ ] **Step 5: Run tests**

```bash
go test ./cmd/routes/... -v
```

Expected: All route tests PASS (existing health/metrics tests still pass).

- [ ] **Step 6: Commit**

```bash
git add cmd/routes/api_routes.go cmd/routes/blackboard_routes.go cmd/routes/blackboard_routes_test.go
git commit -m "feat(routes): add blackboard route registration"
```

---

## Task 8: MCP tools

**Files:**
- Modify: `cmd/mcp/mcp.go`
- Create: `cmd/mcp/blackboard_write_tool.go`
- Create: `cmd/mcp/blackboard_write_tool_test.go`
- Create: `cmd/mcp/blackboard_read_tool.go`
- Create: `cmd/mcp/blackboard_read_tool_test.go`

- [ ] **Step 1: Write the failing tests**

Create `cmd/mcp/blackboard_write_tool_test.go`:

```go
package mcp

import "testing"

func TestBlackboardWriteToolRegistered(t *testing.T) {
	if len(blackboardToolRegistrations) == 0 {
		t.Fatal("blackboardToolRegistrations should not be empty")
	}
}
```

Create `cmd/mcp/blackboard_read_tool_test.go`:

```go
package mcp

import "testing"

func TestBlackboardReadToolRegistered(t *testing.T) {
	found := len(blackboardToolRegistrations) >= 2
	if !found {
		t.Fatalf("expected at least 2 blackboard tool registrations (write + read), got %d", len(blackboardToolRegistrations))
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

```bash
go test ./cmd/mcp/... -run "TestBlackboard" -v
```

Expected: FAIL — `blackboardToolRegistrations` not defined.

- [ ] **Step 3: Update `cmd/mcp/mcp.go`**

Replace the entire file:

```go
package mcp

import (
	"net/http"

	"github.com/martinsuchenak/skopos/internal/blackboard"
	"github.com/martinsuchenak/skopos/internal/status"
	"github.com/paularlott/logger"
	mcplib "github.com/paularlott/mcp"
)

var toolRegistrations []func(*mcplib.Server, *status.Service)
var blackboardToolRegistrations []func(*mcplib.Server, *blackboard.Service)

func RegisterTool(fn func(*mcplib.Server, *status.Service)) {
	toolRegistrations = append(toolRegistrations, fn)
}

func RegisterBlackboardTool(fn func(*mcplib.Server, *blackboard.Service)) {
	blackboardToolRegistrations = append(blackboardToolRegistrations, fn)
}

func StartMCPServer(log logger.Logger, statusService *status.Service, blackboardService *blackboard.Service) {
	server := mcplib.NewServer("skopos-mcp", "1.0.0")

	for _, fn := range toolRegistrations {
		fn(server, statusService)
	}
	for _, fn := range blackboardToolRegistrations {
		fn(server, blackboardService)
	}

	go func() {
		log.Info("starting MCP server on :9000")
		http.HandleFunc("/mcp", server.HandleRequest)
		if err := http.ListenAndServe(":9000", nil); err != nil {
			log.Error("MCP server error", "error", err)
		}
	}()
}
```

- [ ] **Step 4: Create `cmd/mcp/blackboard_write_tool.go`**

```go
package mcp

import (
	"context"

	"github.com/martinsuchenak/skopos/internal/blackboard"
	mcplib "github.com/paularlott/mcp"
)

func init() {
	RegisterBlackboardTool(registerBlackboardWriteTool)
}

func registerBlackboardWriteTool(server *mcplib.Server, service *blackboard.Service) {
	server.RegisterTool(
		mcplib.NewTool("blackboard_write", "Write an entry to the Skopos blackboard memory",
			mcplib.String("scope", "Memory scope: session, branch, or project", mcplib.Required()),
			mcplib.String("entry_type", "Entry type: finding, decision, bug, debt, warning, or context", mcplib.Required()),
			mcplib.String("title", "Short descriptive title", mcplib.Required()),
			mcplib.String("author_agent_id", "Identifier of the writing agent", mcplib.Required()),
			mcplib.String("branch_name", "Branch name (required when scope=branch)"),
			mcplib.String("session_id", "Session ID (required when scope=session)"),
			mcplib.String("content", "Detailed content"),
			mcplib.String("code_ref", "Optional code reference, e.g. auth/jwt.go:45"),
		),
		func(ctx context.Context, req *mcplib.ToolRequest) (*mcplib.ToolResponse, error) {
			input := blackboard.WriteInput{
				Scope:         blackboard.Scope(req.StringOr("scope", "")),
				EntryType:     blackboard.EntryType(req.StringOr("entry_type", "")),
				Title:         req.StringOr("title", ""),
				AuthorAgentID: req.StringOr("author_agent_id", ""),
				BranchName:    req.StringOr("branch_name", ""),
				SessionID:     req.StringOr("session_id", ""),
				Content:       req.StringOr("content", ""),
				CodeRef:       req.StringOr("code_ref", ""),
			}
			result, err := service.Write(ctx, input)
			if err != nil {
				return nil, mcplib.NewToolErrorInvalidParams(err.Error())
			}
			return mcplib.NewToolResponseJSON(result), nil
		},
	)
}
```

- [ ] **Step 5: Create `cmd/mcp/blackboard_read_tool.go`**

```go
package mcp

import (
	"context"

	"github.com/martinsuchenak/skopos/internal/blackboard"
	mcplib "github.com/paularlott/mcp"
)

func init() {
	RegisterBlackboardTool(registerBlackboardReadTool)
}

func registerBlackboardReadTool(server *mcplib.Server, service *blackboard.Service) {
	server.RegisterTool(
		mcplib.NewTool("blackboard_read", "Read the Skopos blackboard Knowledge Bundle",
			mcplib.String("branch", "Branch name to filter branch-scoped entries"),
			mcplib.String("session_id", "Session ID to include session-scoped entries"),
		),
		func(ctx context.Context, req *mcplib.ToolRequest) (*mcplib.ToolResponse, error) {
			bundle, err := service.Bundle(ctx,
				req.StringOr("branch", ""),
				req.StringOr("session_id", ""),
			)
			if err != nil {
				return nil, mcplib.NewToolErrorInvalidParams(err.Error())
			}
			return mcplib.NewToolResponseJSON(bundle), nil
		},
	)
}
```

- [ ] **Step 6: Run all MCP tests**

```bash
go test ./cmd/mcp/... -v
```

Expected: All tests PASS.

- [ ] **Step 7: Commit**

```bash
git add cmd/mcp/mcp.go cmd/mcp/blackboard_write_tool.go cmd/mcp/blackboard_write_tool_test.go \
    cmd/mcp/blackboard_read_tool.go cmd/mcp/blackboard_read_tool_test.go
git commit -m "feat(mcp): add blackboard_write and blackboard_read tools"
```

---

## Task 9: CLI `blackboard` command

**Files:**
- Create: `cmd/blackboard.go`
- Create: `cmd/blackboard_test.go`

- [ ] **Step 1: Write the failing test** in `cmd/blackboard_test.go`

```go
package cmd

import "testing"

func TestBlackboardCmdExists(t *testing.T) {
	cmd := blackboardCmd()
	if cmd == nil {
		t.Fatal("blackboardCmd should not return nil")
	}
	if cmd.Name != "blackboard" {
		t.Fatalf("expected blackboard command, got %q", cmd.Name)
	}
	if len(cmd.Commands) == 0 {
		t.Fatal("expected blackboard subcommands")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
go test ./cmd -run TestBlackboardCmdExists -v
```

Expected: FAIL — `blackboardCmd` not defined.

- [ ] **Step 3: Create `cmd/blackboard.go`**

```go
package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/martinsuchenak/skopos/internal/blackboard"
	"github.com/paularlott/cli"
)

func init() {
	Register(blackboardCmd())
}

func blackboardCmd() *cli.Command {
	return &cli.Command{
		Name:  "blackboard",
		Usage: "Manage shared agent memory on the blackboard",
		Commands: []*cli.Command{
			blackboardWriteCmd(),
			blackboardReadCmd(),
			blackboardListCmd(),
			blackboardPromoteCmd(),
			blackboardDeleteCmd(),
		},
	}
}

func blackboardWriteCmd() *cli.Command {
	return &cli.Command{
		Name:  "write",
		Usage: "Write an entry to the blackboard",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "server-url", DefaultValue: "http://localhost:8080", Usage: "Skopos server URL", EnvVars: []string{"SKOPOS_SERVER_URL"}},
			&cli.StringFlag{Name: "api-key", Usage: "Skopos API key", EnvVars: []string{"SKOPOS_API_KEY"}},
			&cli.StringFlag{Name: "scope", Usage: "session, branch, or project"},
			&cli.StringFlag{Name: "branch", Usage: "Branch name (required for branch scope)"},
			&cli.StringFlag{Name: "session-id", Usage: "Session ID (required for session scope)"},
			&cli.StringFlag{Name: "type", Usage: "Entry type: finding, decision, bug, debt, warning, context"},
			&cli.StringFlag{Name: "title", Usage: "Short descriptive title"},
			&cli.StringFlag{Name: "content", Usage: "Detailed content"},
			&cli.StringFlag{Name: "code-ref", Usage: "Code reference, e.g. auth/jwt.go:45"},
			&cli.StringFlag{Name: "agent-id", Usage: "Agent identifier", EnvVars: []string{"SKOPOS_AGENT_ID"}},
		},
		Run: func(ctx context.Context, cmd *cli.Command) error {
			input := blackboard.WriteInput{
				Scope:         blackboard.Scope(cmd.GetString("scope")),
				BranchName:    cmd.GetString("branch"),
				SessionID:     cmd.GetString("session-id"),
				EntryType:     blackboard.EntryType(cmd.GetString("type")),
				Title:         cmd.GetString("title"),
				Content:       cmd.GetString("content"),
				CodeRef:       cmd.GetString("code-ref"),
				AuthorAgentID: cmd.GetString("agent-id"),
			}
			result, err := blackboardPostEntry(ctx, cmd.GetString("server-url"), cmd.GetString("api-key"), input)
			if err != nil {
				return err
			}
			fmt.Printf("written id=%s scope=%s\n", result.ID, result.Scope)
			return nil
		},
	}
}

func blackboardReadCmd() *cli.Command {
	return &cli.Command{
		Name:  "read",
		Usage: "Print the Knowledge Bundle markdown to stdout",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "server-url", DefaultValue: "http://localhost:8080", EnvVars: []string{"SKOPOS_SERVER_URL"}},
			&cli.StringFlag{Name: "branch", Usage: "Branch name"},
			&cli.StringFlag{Name: "session-id", Usage: "Session ID"},
		},
		Run: func(ctx context.Context, cmd *cli.Command) error {
			bundle, err := blackboardGetBundle(ctx, cmd.GetString("server-url"), cmd.GetString("branch"), cmd.GetString("session-id"))
			if err != nil {
				return err
			}
			fmt.Print(bundle.MarkdownBundle)
			return nil
		},
	}
}

func blackboardListCmd() *cli.Command {
	return &cli.Command{
		Name:  "list",
		Usage: "List blackboard entries in tabular form",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "server-url", DefaultValue: "http://localhost:8080", EnvVars: []string{"SKOPOS_SERVER_URL"}},
			&cli.StringFlag{Name: "branch", Usage: "Branch name"},
			&cli.StringFlag{Name: "session-id", Usage: "Session ID"},
		},
		Run: func(ctx context.Context, cmd *cli.Command) error {
			bundle, err := blackboardGetBundle(ctx, cmd.GetString("server-url"), cmd.GetString("branch"), cmd.GetString("session-id"))
			if err != nil {
				return err
			}
			if len(bundle.Entries) == 0 {
				fmt.Println("no entries")
				return nil
			}
			fmt.Printf("%-36s  %-10s  %-8s  %s\n", "ID", "TYPE", "SCOPE", "TITLE")
			for _, e := range bundle.Entries {
				fmt.Printf("%-36s  %-10s  %-8s  %s\n", e.ID, e.EntryType, e.Scope, e.Title)
			}
			return nil
		},
	}
}

func blackboardPromoteCmd() *cli.Command {
	return &cli.Command{
		Name:  "promote",
		Usage: "Promote an entry to a wider scope",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "server-url", DefaultValue: "http://localhost:8080", EnvVars: []string{"SKOPOS_SERVER_URL"}},
			&cli.StringFlag{Name: "api-key", Usage: "Skopos API key", EnvVars: []string{"SKOPOS_API_KEY"}},
			&cli.StringFlag{Name: "id", Usage: "Entry ID to promote"},
		},
		Run: func(ctx context.Context, cmd *cli.Command) error {
			id := cmd.GetString("id")
			if strings.TrimSpace(id) == "" {
				return fmt.Errorf("--id is required")
			}
			return blackboardPatchPromote(ctx, cmd.GetString("server-url"), cmd.GetString("api-key"), id)
		},
	}
}

func blackboardDeleteCmd() *cli.Command {
	return &cli.Command{
		Name:  "delete",
		Usage: "Delete a blackboard entry",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "server-url", DefaultValue: "http://localhost:8080", EnvVars: []string{"SKOPOS_SERVER_URL"}},
			&cli.StringFlag{Name: "api-key", Usage: "Skopos API key", EnvVars: []string{"SKOPOS_API_KEY"}},
			&cli.StringFlag{Name: "id", Usage: "Entry ID to delete"},
		},
		Run: func(ctx context.Context, cmd *cli.Command) error {
			id := cmd.GetString("id")
			if strings.TrimSpace(id) == "" {
				return fmt.Errorf("--id is required")
			}
			return blackboardDoDelete(ctx, cmd.GetString("server-url"), cmd.GetString("api-key"), id)
		},
	}
}

func blackboardPostEntry(ctx context.Context, serverURL, apiKey string, input blackboard.WriteInput) (*blackboard.WriteResult, error) {
	payload, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("encoding input: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(serverURL, "/")+"/api/blackboard/entries",
		bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("X-API-Key", apiKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("posting entry: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("posting entry: unexpected status %s", resp.Status)
	}
	var result blackboard.WriteResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}
	return &result, nil
}

func blackboardGetBundle(ctx context.Context, serverURL, branch, sessionID string) (*blackboard.Bundle, error) {
	u := strings.TrimRight(serverURL, "/") + "/api/blackboard/entries"
	params := []string{}
	if branch != "" {
		params = append(params, "branch="+branch)
	}
	if sessionID != "" {
		params = append(params, "session_id="+sessionID)
	}
	if len(params) > 0 {
		u += "?" + strings.Join(params, "&")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching bundle: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetching bundle: unexpected status %s", resp.Status)
	}
	var bundle blackboard.Bundle
	if err := json.NewDecoder(resp.Body).Decode(&bundle); err != nil {
		return nil, fmt.Errorf("decoding bundle: %w", err)
	}
	return &bundle, nil
}

func blackboardPatchPromote(ctx context.Context, serverURL, apiKey, id string) error {
	u := strings.TrimRight(serverURL, "/") + "/api/blackboard/entries/" + id + "/promote"
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, u, nil)
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	if apiKey != "" {
		req.Header.Set("X-API-Key", apiKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("promoting entry: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("promoting entry: unexpected status %s", resp.Status)
	}
	fmt.Printf("promoted %s\n", id)
	return nil
}

func blackboardDoDelete(ctx context.Context, serverURL, apiKey, id string) error {
	u := strings.TrimRight(serverURL, "/") + "/api/blackboard/entries/" + id
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, u, nil)
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	if apiKey != "" {
		req.Header.Set("X-API-Key", apiKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("deleting entry: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("deleting entry: unexpected status %s", resp.Status)
	}
	fmt.Printf("deleted %s\n", id)
	return nil
}
```

- [ ] **Step 4: Run tests**

```bash
go test ./cmd -run TestBlackboardCmdExists -v
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/blackboard.go cmd/blackboard_test.go
git commit -m "feat(cli): add blackboard command group (write/read/list/promote/delete)"
```

---

## Task 10: Wire into `cmd/serve.go`

**Files:**
- Modify: `cmd/serve.go`

- [ ] **Step 1: Update `cmd/serve.go`**

Add the `blackboard` import to the import block (after `"github.com/martinsuchenak/skopos/internal/health"`):

```go
"github.com/martinsuchenak/skopos/internal/blackboard"
```

In the `Run` function, after wiring the status service (after `statusHandler := ...`), add blackboard wiring:

```go
blackboardStorage := blackboard.NewStorage(conn.SQL)
blackboardService := blackboard.NewService(blackboardStorage)
blackboardHandler := blackboard.NewHandler(blackboardService, cmd.GetString("api-key"))
```

Update the `StartMCPServer` call to pass the blackboard service:

```go
mcpserver.StartMCPServer(log, statusService, blackboardService)
```

Update the `RegisterRoutes` call to pass the blackboard handler:

```go
routes.RegisterRoutes(mux, statusHandler, blackboardHandler)
```

The final `serve.go` Run body should look like:

```go
Run: func(ctx context.Context, cmd *cli.Command) error {
    log := logslog.New(logslog.Config{
        Level:  cmd.GetString("log-level"),
        Format: cmd.GetString("log-format"),
        Writer: os.Stdout,
    })
    log.Info("starting skopos service")

    conn, err := db.Connect(log, "localhost:0", "", "", cmd.GetString("database-path"))
    if err != nil {
        return err
    }
    defer conn.SQL.Close()
    if err := db.RunMigrations(conn.SQL); err != nil {
        return err
    }

    statusService := status.NewService(status.NewStorage(conn.SQL))
    statusHandler := status.NewHandler(statusService, cmd.GetString("api-key"))

    blackboardStorage := blackboard.NewStorage(conn.SQL)
    blackboardService := blackboard.NewService(blackboardStorage)
    blackboardHandler := blackboard.NewHandler(blackboardService, cmd.GetString("api-key"))

    mcpserver.StartMCPServer(log, statusService, blackboardService)

    threshold := time.Duration(cmd.GetInt("health-stuck-threshold")) * time.Minute
    health.NewChecker(conn.SQL, threshold, log).Start(ctx)
    // go-scaffolder:serve-init

    mux := http.NewServeMux()
    routes.RegisterRoutes(mux, statusHandler, blackboardHandler)

    addr := fmt.Sprintf("%s:%d", cmd.GetString("server-host"), cmd.GetInt("server-port"))
    log.Info("starting HTTP server", "addr", addr)
    // go-scaffolder:serve-start
    return http.ListenAndServe(addr, mux)
},
```

- [ ] **Step 2: Build to verify no errors**

```bash
go build ./...
```

Expected: no output (success).

- [ ] **Step 3: Run the full test suite**

```bash
go test ./... -count=1
```

Expected: all tests pass.

- [ ] **Step 4: Commit**

```bash
git add cmd/serve.go
git commit -m "feat(serve): wire blackboard storage, service, handler into server"
```

---

## Final verification

- [ ] **Build the binary**

```bash
go build -o /tmp/skopos .
```

Expected: binary produced.

- [ ] **Smoke-test the CLI**

```bash
/tmp/skopos blackboard --help
```

Expected: shows `write`, `read`, `list`, `promote`, `delete` subcommands.

- [ ] **Run full suite one last time**

```bash
go test ./... -count=1 -race
```

Expected: all tests pass, no races.
