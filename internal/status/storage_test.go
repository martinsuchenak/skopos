package status

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/martinsuchenak/skopos/internal/db"
	_ "modernc.org/sqlite"
)

func TestModelsHaveStuckAndOrphanedStatus(t *testing.T) {
	_ = StatusStuck
	_ = StatusOrphaned
	var state AgentState
	_ = state.OriginalStatus
	_ = state.StuckAt
}

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

func TestStorageRecordReportCreatesSessionAgentStateAndEvent(t *testing.T) {
	storage := testStorage(t)
	progress := 25
	now := time.Date(2026, 5, 10, 4, 5, 6, 0, time.UTC)

	err := storage.RecordReport(context.Background(), Event{
		ID:        "event-1",
		SessionID: "session-1",
		AgentID:   "codex-1",
		AgentType: "codex",
		Workspace: "/repo",
		Status:    StatusRunning,
		Progress:  &progress,
		Message:   "running tests",
		Metadata:  map[string]any{"branch": "main"},
		CreatedAt: now,
	}, "/repo")
	if err != nil {
		t.Fatalf("record report: %v", err)
	}

	sessions, err := storage.ListSessions(context.Background(), "")
	if err != nil {
		t.Fatalf("list sessions: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(sessions))
	}
	if sessions[0].AgentCount != 1 {
		t.Fatalf("expected 1 agent, got %d", sessions[0].AgentCount)
	}

	detail, err := storage.GetSession(context.Background(), "session-1")
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if len(detail.Agents) != 1 {
		t.Fatalf("expected 1 agent state, got %d", len(detail.Agents))
	}
	if len(detail.Events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(detail.Events))
	}
	if detail.Agents[0].Progress == nil || *detail.Agents[0].Progress != progress {
		t.Fatalf("expected progress %d, got %#v", progress, detail.Agents[0].Progress)
	}
	if detail.Events[0].Metadata["branch"] != "main" {
		t.Fatalf("expected metadata branch main, got %#v", detail.Events[0].Metadata)
	}
}

func TestStorageRecordReportUpdatesLatestAgentState(t *testing.T) {
	storage := testStorage(t)
	ctx := context.Background()

	for _, report := range []Event{
		{ID: "event-1", SessionID: "session-1", AgentID: "agent-1", AgentType: "codex", Workspace: "/repo", Status: StatusRunning, Message: "start", Metadata: map[string]any{}, CreatedAt: time.Now()},
		{ID: "event-2", SessionID: "session-1", AgentID: "agent-1", AgentType: "codex", Workspace: "/repo", Status: StatusSucceeded, Message: "done", Metadata: map[string]any{}, CreatedAt: time.Now().Add(time.Second)},
	} {
		if err := storage.RecordReport(ctx, report, "/repo"); err != nil {
			t.Fatalf("record report: %v", err)
		}
	}

	detail, err := storage.GetSession(ctx, "session-1")
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if len(detail.Agents) != 1 {
		t.Fatalf("expected one latest state, got %d", len(detail.Agents))
	}
	if detail.Agents[0].Status != StatusSucceeded {
		t.Fatalf("expected latest status succeeded, got %s", detail.Agents[0].Status)
	}
	if len(detail.Events) != 2 {
		t.Fatalf("expected event history, got %d", len(detail.Events))
	}
}

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
		GitBranch: "feat-auth",
	}, "/repo")
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
