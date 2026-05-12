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

	sessions, err := storage.ListSessions(context.Background())
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
