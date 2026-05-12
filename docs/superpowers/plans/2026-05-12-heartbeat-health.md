# Heartbeat & Health Monitoring Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add passive staleness detection — a background ticker marks silent agents as `stuck` and sessions whose every agent is stuck/terminal as `orphaned`, surfaced via the existing API with no new endpoints.

**Architecture:** A new `internal/health` package runs a 60-second ticker in a background goroutine started from `serve.go`. Each sweep runs in one transaction: first queries and marks stale active agents `stuck` (copying their last status to `original_status`), inserts synthetic events for each, then marks any session where every agent is stuck/terminal as `orphaned`. Recovery is automatic — any new report from an agent clears `original_status` and `stuck_at` via the existing upsert in `RecordReport`.

**Tech Stack:** Go, SQLite (modernc.org/sqlite), github.com/google/uuid (already in go.mod), Alpine.js/Tailwind

---

## File Map

| File | Action | Purpose |
|---|---|---|
| `internal/db/schema.sql` | Modify | Add `original_status TEXT` and `stuck_at TEXT` to `agent_states` |
| `internal/status/models.go` | Modify | Add `StatusStuck`, `StatusOrphaned`; add `OriginalStatus`, `StuckAt` to `AgentState` |
| `internal/status/storage.go` | Modify | Upsert clears stuck fields on recovery; scan + query include new columns |
| `internal/status/storage_test.go` | Modify | Add recovery test; add compile-time model check |
| `internal/health/ticker.go` | Create | `Checker` struct with `Start` and `check` methods |
| `internal/health/ticker_test.go` | Create | 7 test cases against `:memory:` SQLite |
| `skopos-config.toml` | Modify | Add `[health]` section with `stuck_threshold_minutes = 15` |
| `cmd/serve.go` | Modify | Add `--health-stuck-threshold` flag; wire `health.Checker` |
| `web/src/main.ts` | Modify | Add `stuck` (amber) and `orphaned` (rose-muted) to `statusClass` |

---

### Task 1: Schema — add `original_status` and `stuck_at` to `agent_states`

**Files:**
- Modify: `internal/db/schema.sql`

The DB is wiped and recreated from schema.sql on each fresh start — no migration needed.

- [ ] **Step 1: Update `agent_states` table in schema.sql**

Replace the `agent_states` CREATE TABLE block with:

```sql
CREATE TABLE IF NOT EXISTS agent_states (
    session_id TEXT NOT NULL,
    agent_id TEXT NOT NULL,
    agent_type TEXT NOT NULL,
    workspace TEXT NOT NULL,
    status TEXT NOT NULL,
    progress INTEGER,
    step_current INTEGER,
    step_total INTEGER,
    message TEXT NOT NULL DEFAULT '',
    snippet TEXT NOT NULL DEFAULT '',
    metadata TEXT NOT NULL DEFAULT '{}',
    updated_at TEXT NOT NULL,
    original_status TEXT,
    stuck_at        TEXT,
    PRIMARY KEY (session_id, agent_id),
    FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE CASCADE,
    FOREIGN KEY (agent_id) REFERENCES agents(id) ON DELETE CASCADE
);
```

- [ ] **Step 2: Verify the db package compiles**

Run: `go build ./internal/db/...`
Expected: no output (success)

- [ ] **Step 3: Commit**

```bash
git add internal/db/schema.sql
git commit -m "feat: add original_status and stuck_at columns to agent_states schema"
```

---

### Task 2: Models — add status constants and `AgentState` fields

**Files:**
- Modify: `internal/status/models.go`
- Modify: `internal/status/storage_test.go`

- [ ] **Step 1: Write the failing compile-time test**

In `internal/status/storage_test.go`, add after the existing imports:

```go
func TestModelsHaveStuckAndOrphanedStatus(t *testing.T) {
	_ = StatusStuck
	_ = StatusOrphaned
	var state AgentState
	_ = state.OriginalStatus
	_ = state.StuckAt
}
```

- [ ] **Step 2: Run to confirm it fails**

Run: `go test ./internal/status/... -run TestModelsHaveStuckAndOrphanedStatus -v`
Expected: FAIL — `undefined: StatusStuck`

- [ ] **Step 3: Add `StatusStuck` and `StatusOrphaned` constants to models.go**

In `internal/status/models.go`, extend the `const` block (after `StatusCancelled`):

```go
const (
	StatusPending   Status = "pending"
	StatusThinking  Status = "thinking"
	StatusPlanning  Status = "planning"
	StatusRunning   Status = "running"
	StatusEditing   Status = "editing"
	StatusTesting   Status = "testing"
	StatusWaiting   Status = "waiting"
	StatusBlocked   Status = "blocked"
	StatusPaused    Status = "paused"
	StatusHandoff   Status = "handoff"
	StatusSucceeded Status = "succeeded"
	StatusFailed    Status = "failed"
	StatusCancelled Status = "cancelled"
	StatusStuck     Status = "stuck"
	StatusOrphaned  Status = "orphaned"
)
```

- [ ] **Step 4: Add `OriginalStatus` and `StuckAt` fields to `AgentState`**

In `internal/status/models.go`, replace the `AgentState` struct:

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
}
```

- [ ] **Step 5: Run the test to confirm it passes**

Run: `go test ./internal/status/... -run TestModelsHaveStuckAndOrphanedStatus -v`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add internal/status/models.go internal/status/storage_test.go
git commit -m "feat: add StatusStuck, StatusOrphaned constants and OriginalStatus/StuckAt to AgentState"
```

---

### Task 3: Storage — update upsert (recovery), query, and scan

**Files:**
- Modify: `internal/status/storage.go`
- Modify: `internal/status/storage_test.go`

Three changes in this task:
1. `RecordReport` upsert clears `original_status`/`stuck_at` on every conflict resolution (recovery)
2. `listAgentStates` query selects the two new columns
3. `scanAgentState` scans them into nullable strings, populates the new struct fields

**Note:** `validStatus()` in `service.go` already excludes `stuck` and `orphaned` — agents cannot self-report these. No changes needed there.

- [ ] **Step 1: Write the failing recovery test**

In `internal/status/storage_test.go`, add:

```go
func TestStorageRecordReportClearsStuckFields(t *testing.T) {
	storage := testStorage(t)
	ctx := context.Background()
	now := time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC)

	// Create the initial agent state
	err := storage.RecordReport(ctx, Event{
		ID:        "event-1",
		SessionID: "session-1",
		AgentID:   "agent-1",
		AgentType: "codex",
		Workspace: "/repo",
		Status:    StatusRunning,
		Message:   "running",
		Metadata:  map[string]any{},
		CreatedAt: now,
	}, "/repo")
	if err != nil {
		t.Fatalf("record initial report: %v", err)
	}

	// Simulate the health ticker writing stuck state directly
	_, err = storage.db.ExecContext(ctx,
		`UPDATE agent_states SET original_status = 'running', status = 'stuck', stuck_at = '2026-05-12T09:00:00Z'
		 WHERE session_id = 'session-1' AND agent_id = 'agent-1'`)
	if err != nil {
		t.Fatalf("inject stuck state: %v", err)
	}

	// Agent sends a new report — RecordReport upsert should clear original_status and stuck_at
	err = storage.RecordReport(ctx, Event{
		ID:        "event-2",
		SessionID: "session-1",
		AgentID:   "agent-1",
		AgentType: "codex",
		Workspace: "/repo",
		Status:    StatusRunning,
		Message:   "back online",
		Metadata:  map[string]any{},
		CreatedAt: now.Add(time.Minute),
	}, "/repo")
	if err != nil {
		t.Fatalf("record recovery report: %v", err)
	}

	session, err := storage.GetSession(ctx, "session-1")
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if len(session.Agents) != 1 {
		t.Fatalf("expected 1 agent, got %d", len(session.Agents))
	}
	agent := session.Agents[0]
	if agent.Status != StatusRunning {
		t.Errorf("status: got %q, want %q", agent.Status, StatusRunning)
	}
	if agent.OriginalStatus != nil {
		t.Errorf("original_status: got %q, want nil", *agent.OriginalStatus)
	}
	if agent.StuckAt != nil {
		t.Errorf("stuck_at: got %v, want nil", *agent.StuckAt)
	}
}
```

- [ ] **Step 2: Run to confirm it fails**

Run: `go test ./internal/status/... -run TestStorageRecordReportClearsStuckFields -v`
Expected: FAIL — scan error (wrong column count) or `OriginalStatus` not nil after recovery

- [ ] **Step 3: Update `RecordReport` upsert to include and clear the new columns**

In `internal/status/storage.go`, replace the `agent_states` INSERT block (lines 55–76):

```go
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO agent_states (
			session_id, agent_id, agent_type, workspace, status, progress, step_current,
			step_total, message, snippet, metadata, updated_at, original_status, stuck_at
		)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, NULL)
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
			stuck_at = NULL
	`, report.SessionID, report.AgentID, report.AgentType, report.Workspace, string(report.Status),
		nullableInt(report.Progress), nullableInt(report.StepCurrent), nullableInt(report.StepTotal),
		report.Message, report.Snippet, string(metadata), now); err != nil {
		return fmt.Errorf("upserting agent state: %w", err)
	}
```

- [ ] **Step 4: Update `listAgentStates` query to select the new columns**

In `internal/status/storage.go`, replace the query in `listAgentStates` (lines 184–189):

```go
	rows, err := s.db.QueryContext(ctx, `
		SELECT session_id, agent_id, agent_type, workspace, status, progress, step_current,
			step_total, message, snippet, metadata, updated_at, original_status, stuck_at
		FROM agent_states
		WHERE session_id = ?
		ORDER BY updated_at DESC
	`, sessionID)
```

- [ ] **Step 5: Update `scanAgentState` to scan the two new nullable columns**

In `internal/status/storage.go`, replace `scanAgentState` (lines 230–244):

```go
func scanAgentState(row rowScanner) (AgentState, error) {
	var state AgentState
	var progress, stepCurrent, stepTotal sql.NullInt64
	var metadata, updatedAt string
	var originalStatus, stuckAt sql.NullString
	if err := row.Scan(
		&state.SessionID, &state.AgentID, &state.AgentType, &state.Workspace,
		&state.Status, &progress, &stepCurrent, &stepTotal, &state.Message, &state.Snippet,
		&metadata, &updatedAt, &originalStatus, &stuckAt,
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
	return state, nil
}
```

- [ ] **Step 6: Run all status tests**

Run: `go test ./internal/status/... -v -count=1`
Expected: all tests PASS

- [ ] **Step 7: Commit**

```bash
git add internal/status/storage.go internal/status/storage_test.go
git commit -m "feat: update storage upsert, query and scan for original_status/stuck_at"
```

---

### Task 4: Health ticker — create `internal/health/` package

**Files:**
- Create: `internal/health/ticker_test.go`
- Create: `internal/health/ticker.go`

Write tests first. The `Checker` struct exposes `now func() time.Time` (unexported field, accessible within the package) for deterministic time injection in tests.

- [ ] **Step 1: Create `internal/health/ticker_test.go`**

```go
package health

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/martinsuchenak/skopos/internal/db"
	_ "modernc.org/sqlite"
)

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	sqlDB, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	if err := db.RunMigrations(sqlDB); err != nil {
		t.Fatalf("run migrations: %v", err)
	}
	return sqlDB
}

func seedAgentState(t *testing.T, sqlDB *sql.DB, sessionID, agentID, agentType, sessionStatus, agentStatus, updatedAt string) {
	t.Helper()
	_, err := sqlDB.Exec(`
		INSERT OR IGNORE INTO sessions (id, title, workspace, status, started_at, updated_at)
		VALUES (?, ?, '/test', ?, ?, ?)
	`, sessionID, sessionID, sessionStatus, updatedAt, updatedAt)
	if err != nil {
		t.Fatalf("seed session: %v", err)
	}
	_, err = sqlDB.Exec(`
		INSERT OR IGNORE INTO agents (id, type, workspace, first_seen_at, last_seen_at)
		VALUES (?, ?, '/test', ?, ?)
	`, agentID, agentType, updatedAt, updatedAt)
	if err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	_, err = sqlDB.Exec(`
		INSERT INTO agent_states (session_id, agent_id, agent_type, workspace, status, message, snippet, metadata, updated_at)
		VALUES (?, ?, ?, '/test', ?, '', '', '{}', ?)
	`, sessionID, agentID, agentType, agentStatus, updatedAt)
	if err != nil {
		t.Fatalf("seed agent state: %v", err)
	}
}

func checkerAt(sqlDB *sql.DB, now time.Time) *Checker {
	c := NewChecker(sqlDB, time.Minute)
	c.now = func() time.Time { return now }
	return c
}

func TestCheckerMarksStaleActiveAgentStuck(t *testing.T) {
	sqlDB := testDB(t)
	now := time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC)
	staleTime := formatTime(now.Add(-2 * time.Minute)) // 2 min ago; threshold is 1 min

	seedAgentState(t, sqlDB, "s1", "a1", "codex", "running", "running", staleTime)

	if err := checkerAt(sqlDB, now).check(context.Background()); err != nil {
		t.Fatalf("check: %v", err)
	}

	var status, originalStatus, stuckAt sql.NullString
	sqlDB.QueryRow(`SELECT status, original_status, stuck_at FROM agent_states WHERE agent_id = 'a1'`).
		Scan(&status, &originalStatus, &stuckAt)
	if status.String != "stuck" {
		t.Errorf("status: got %q, want %q", status.String, "stuck")
	}
	if originalStatus.String != "running" {
		t.Errorf("original_status: got %q, want %q", originalStatus.String, "running")
	}
	if !stuckAt.Valid {
		t.Error("stuck_at: want non-null")
	}

	var eventCount int
	sqlDB.QueryRow(`SELECT COUNT(*) FROM events WHERE agent_id = 'a1' AND status = 'stuck'`).Scan(&eventCount)
	if eventCount != 1 {
		t.Errorf("stuck event count: got %d, want 1", eventCount)
	}
}

func TestCheckerDoesNotMarkFreshAgentStuck(t *testing.T) {
	sqlDB := testDB(t)
	now := time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC)
	freshTime := formatTime(now.Add(-30 * time.Second)) // 30s ago; threshold is 1 min

	seedAgentState(t, sqlDB, "s1", "a1", "codex", "running", "running", freshTime)

	if err := checkerAt(sqlDB, now).check(context.Background()); err != nil {
		t.Fatalf("check: %v", err)
	}

	var status string
	sqlDB.QueryRow(`SELECT status FROM agent_states WHERE agent_id = 'a1'`).Scan(&status)
	if status != "running" {
		t.Errorf("status: got %q, want %q", status, "running")
	}
}

func TestCheckerDoesNotMarkWaitingAgentStuck(t *testing.T) {
	sqlDB := testDB(t)
	now := time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC)
	staleTime := formatTime(now.Add(-2 * time.Minute))

	seedAgentState(t, sqlDB, "s1", "a1", "codex", "running", "waiting", staleTime)

	if err := checkerAt(sqlDB, now).check(context.Background()); err != nil {
		t.Fatalf("check: %v", err)
	}

	var status string
	sqlDB.QueryRow(`SELECT status FROM agent_states WHERE agent_id = 'a1'`).Scan(&status)
	if status != "waiting" {
		t.Errorf("status: got %q, want %q", status, "waiting")
	}
}

func TestCheckerDoesNotMarkTerminalAgentStuck(t *testing.T) {
	sqlDB := testDB(t)
	now := time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC)
	staleTime := formatTime(now.Add(-2 * time.Minute))

	seedAgentState(t, sqlDB, "s1", "a1", "codex", "succeeded", "succeeded", staleTime)

	if err := checkerAt(sqlDB, now).check(context.Background()); err != nil {
		t.Fatalf("check: %v", err)
	}

	var status string
	sqlDB.QueryRow(`SELECT status FROM agent_states WHERE agent_id = 'a1'`).Scan(&status)
	if status != "succeeded" {
		t.Errorf("status: got %q, want %q", status, "succeeded")
	}
}

func TestCheckerMarksSessionOrphanedWhenAllAgentsStuckOrTerminal(t *testing.T) {
	sqlDB := testDB(t)
	now := time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC)
	staleTime := formatTime(now.Add(-2 * time.Minute))

	// a1: running → will be marked stuck; a2: already terminal
	seedAgentState(t, sqlDB, "s1", "a1", "codex", "running", "running", staleTime)
	seedAgentState(t, sqlDB, "s1", "a2", "gemini", "running", "succeeded", staleTime)

	if err := checkerAt(sqlDB, now).check(context.Background()); err != nil {
		t.Fatalf("check: %v", err)
	}

	var sessionStatus string
	sqlDB.QueryRow(`SELECT status FROM sessions WHERE id = 's1'`).Scan(&sessionStatus)
	if sessionStatus != "orphaned" {
		t.Errorf("session status: got %q, want %q", sessionStatus, "orphaned")
	}
}

func TestCheckerDoesNotOrphanSessionWithActiveAgent(t *testing.T) {
	sqlDB := testDB(t)
	now := time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC)
	staleTime := formatTime(now.Add(-2 * time.Minute))
	freshTime := formatTime(now.Add(-30 * time.Second))

	// a1: stale → will be stuck; a2: fresh → still running, session not orphaned
	seedAgentState(t, sqlDB, "s1", "a1", "codex", "running", "running", staleTime)
	seedAgentState(t, sqlDB, "s1", "a2", "gemini", "running", "running", freshTime)

	if err := checkerAt(sqlDB, now).check(context.Background()); err != nil {
		t.Fatalf("check: %v", err)
	}

	var sessionStatus string
	sqlDB.QueryRow(`SELECT status FROM sessions WHERE id = 's1'`).Scan(&sessionStatus)
	if sessionStatus != "running" {
		t.Errorf("session status: got %q, want %q", sessionStatus, "running")
	}
}

func TestCheckerRecovery(t *testing.T) {
	sqlDB := testDB(t)
	now := time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC)
	staleTime := formatTime(now.Add(-2 * time.Minute))

	seedAgentState(t, sqlDB, "s1", "a1", "codex", "running", "running", staleTime)

	// First check: marks agent stuck
	if err := checkerAt(sqlDB, now).check(context.Background()); err != nil {
		t.Fatalf("first check: %v", err)
	}

	// Simulate recovery: agent sends a new report (storage upsert would do this)
	sqlDB.Exec(
		`UPDATE agent_states SET status = 'running', original_status = NULL, stuck_at = NULL, updated_at = ?
		 WHERE agent_id = 'a1'`,
		formatTime(now.Add(time.Second)),
	)

	// Second check with now+1s: agent is fresh, should not re-mark stuck
	if err := checkerAt(sqlDB, now.Add(time.Second)).check(context.Background()); err != nil {
		t.Fatalf("second check: %v", err)
	}

	var status string
	var originalStatus, stuckAt sql.NullString
	sqlDB.QueryRow(`SELECT status, original_status, stuck_at FROM agent_states WHERE agent_id = 'a1'`).
		Scan(&status, &originalStatus, &stuckAt)

	if status != "running" {
		t.Errorf("status after recovery: got %q, want %q", status, "running")
	}
	if originalStatus.Valid {
		t.Errorf("original_status after recovery: want nil, got %q", originalStatus.String)
	}
	if stuckAt.Valid {
		t.Errorf("stuck_at after recovery: want nil, got %q", stuckAt.String)
	}
}
```

- [ ] **Step 2: Run to confirm all tests fail (package does not exist)**

Run: `go test ./internal/health/... -v -count=1`
Expected: FAIL — cannot find package

- [ ] **Step 3: Create `internal/health/ticker.go`**

```go
package health

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"
)

type Checker struct {
	db        *sql.DB
	threshold time.Duration
	interval  time.Duration
	now       func() time.Time
}

func NewChecker(db *sql.DB, threshold time.Duration) *Checker {
	return &Checker{
		db:        db,
		threshold: threshold,
		interval:  60 * time.Second,
		now:       time.Now,
	}
}

func (c *Checker) Start(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(c.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				_ = c.check(ctx)
			case <-ctx.Done():
				return
			}
		}
	}()
}

func (c *Checker) check(ctx context.Context) error {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	now := formatTime(c.now().UTC())
	staleTime := formatTime(c.now().UTC().Add(-c.threshold))

	rows, err := tx.QueryContext(ctx, `
		SELECT session_id, agent_id, agent_type, workspace
		FROM agent_states
		WHERE status IN ('pending', 'running', 'thinking', 'planning', 'editing', 'testing')
		AND updated_at < ?
		AND stuck_at IS NULL
	`, staleTime)
	if err != nil {
		return fmt.Errorf("querying stale agents: %w", err)
	}

	type staleAgent struct{ sessionID, agentID, agentType, workspace string }
	var stale []staleAgent
	for rows.Next() {
		var a staleAgent
		if err := rows.Scan(&a.sessionID, &a.agentID, &a.agentType, &a.workspace); err != nil {
			rows.Close()
			return fmt.Errorf("scanning stale agent: %w", err)
		}
		stale = append(stale, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterating stale agents: %w", err)
	}

	for _, a := range stale {
		if _, err := tx.ExecContext(ctx, `
			UPDATE agent_states
			SET original_status = status, status = 'stuck', stuck_at = ?
			WHERE session_id = ? AND agent_id = ?
		`, now, a.sessionID, a.agentID); err != nil {
			return fmt.Errorf("marking agent stuck: %w", err)
		}

		id, _ := uuid.NewV7()
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO events (id, session_id, agent_id, agent_type, workspace, status, message, snippet, metadata, created_at)
			VALUES (?, ?, ?, ?, ?, 'stuck', 'agent not responding', '', '{}', ?)
		`, id.String(), a.sessionID, a.agentID, a.agentType, a.workspace, now); err != nil {
			return fmt.Errorf("inserting stuck event: %w", err)
		}
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE sessions
		SET status = 'orphaned', updated_at = ?
		WHERE status NOT IN ('succeeded', 'failed', 'cancelled', 'orphaned')
		AND EXISTS (
			SELECT 1 FROM agent_states WHERE agent_states.session_id = sessions.id
		)
		AND NOT EXISTS (
			SELECT 1 FROM agent_states
			WHERE agent_states.session_id = sessions.id
			AND agent_states.status NOT IN ('stuck', 'succeeded', 'failed', 'cancelled', 'handoff')
		)
	`, now); err != nil {
		return fmt.Errorf("marking orphaned sessions: %w", err)
	}

	return tx.Commit()
}

func formatTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}
```

- [ ] **Step 4: Run tests to confirm all 7 pass**

Run: `go test ./internal/health/... -v -count=1`
Expected: all 7 tests PASS

- [ ] **Step 5: Commit**

```bash
git add internal/health/ticker.go internal/health/ticker_test.go
git commit -m "feat: add internal/health package with staleness checker"
```

---

### Task 5: Config and wiring

**Files:**
- Modify: `skopos-config.toml`
- Modify: `cmd/serve.go`

- [ ] **Step 1: Add `[health]` section to skopos-config.toml**

In `skopos-config.toml`, add before `# go-scaffolder:config-sections`:

```toml
[health]
stuck_threshold_minutes = 15
```

Full file after change:

```toml
[log]
level = "info"
format = "text"

[server]
host = "0.0.0.0"
port = 8080

[database]
path = "skopos.db"

[auth]
api_key = ""

[valkey]
host = "localhost"
port = 6379
password = ""

[health]
stuck_threshold_minutes = 15
# go-scaffolder:config-sections
```

- [ ] **Step 2: Add `--health-stuck-threshold` flag in cmd/serve.go**

In `cmd/serve.go`, add after the `api-key` flag entry (before `// go-scaffolder:serve-flags`):

```go
			&cli.IntFlag{
				Name:         "health-stuck-threshold",
				DefaultValue: 15,
				Usage:        "Minutes before an active agent is marked stuck",
				ConfigPath:   []string{"health.stuck_threshold_minutes"},
				EnvVars:      []string{"HEALTH_STUCK_THRESHOLD"},
			},
```

- [ ] **Step 3: Add `"time"` import and health package import in cmd/serve.go**

Update the import block in `cmd/serve.go`:

```go
import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/paularlott/cli"
	logslog "github.com/paularlott/logger/slog"

	"github.com/martinsuchenak/skopos/cmd/routes"
	"github.com/martinsuchenak/skopos/internal/db"
	"github.com/martinsuchenak/skopos/internal/health"
	"github.com/martinsuchenak/skopos/internal/status"

	mcpserver "github.com/martinsuchenak/skopos/cmd/mcp"
	// go-scaffolder:serve-imports
)
```

- [ ] **Step 4: Wire the health checker in the Run function**

In `cmd/serve.go`, in the `Run` function add after `mcpserver.StartMCPServer(log, statusService)` (before `// go-scaffolder:serve-init`):

```go
			threshold := time.Duration(cmd.GetInt("health-stuck-threshold")) * time.Minute
			health.NewChecker(conn.SQL, threshold).Start(ctx)
```

- [ ] **Step 5: Build to confirm no compile errors**

Run: `go build ./...`
Expected: no output (success)

- [ ] **Step 6: Run all tests**

Run: `task test`
Expected: all tests PASS

- [ ] **Step 7: Commit**

```bash
git add skopos-config.toml cmd/serve.go
git commit -m "feat: add --health-stuck-threshold flag and wire health.Checker in serve"
```

---

### Task 6: Dashboard — add `stuck` and `orphaned` badge styles

**Files:**
- Modify: `web/src/main.ts`

- [ ] **Step 1: Update `AgentState` TypeScript type to include stuck fields**

In `web/src/main.ts`, replace the `AgentState` type:

```typescript
type AgentState = {
  agent_id: string;
  agent_type: string;
  status: string;
  progress?: number;
  message: string;
  snippet: string;
  original_status?: string;
  stuck_at?: string;
};
```

- [ ] **Step 2: Add `stuck` and `orphaned` cases to `statusClass`**

In `web/src/main.ts`, replace the `statusClass` function:

```typescript
  statusClass(status?: string) {
    switch (status) {
      case 'succeeded':
        return 'bg-emerald-500/15 text-emerald-300';
      case 'failed':
      case 'blocked':
        return 'bg-rose-500/15 text-rose-300';
      case 'orphaned':
        return 'bg-rose-500/15 text-rose-200';
      case 'testing':
      case 'running':
      case 'editing':
        return 'bg-cyan-500/15 text-cyan-300';
      case 'waiting':
      case 'paused':
      case 'stuck':
        return 'bg-amber-500/15 text-amber-300';
      default:
        return 'bg-zinc-700 text-zinc-200';
    }
  },
```

- [ ] **Step 3: Build the frontend**

Run: `cd web && bun run build`
Expected: build succeeds with no TypeScript errors

- [ ] **Step 4: Commit**

```bash
git add web/src/main.ts
git commit -m "feat: add stuck (amber) and orphaned (rose-muted) badge styles to dashboard"
```

---

## Final Verification

- [ ] **Run the full test suite**

Run: `task test`
Expected: all tests PASS, including:
- `internal/status/...` — existing tests + `TestModelsHaveStuckAndOrphanedStatus` + `TestStorageRecordReportClearsStuckFields`
- `internal/health/...` — 7 ticker tests

- [ ] **Build the full binary**

Run: `go build ./...`
Expected: no errors
