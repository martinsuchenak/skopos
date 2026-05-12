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
