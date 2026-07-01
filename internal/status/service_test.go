package status

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fakeStore struct {
	report Event
	title  string
}

func (s *fakeStore) RecordReport(ctx context.Context, report Event, sessionTitle string) error {
	s.report = report
	s.title = sessionTitle
	return nil
}

func (s *fakeStore) ListSessions(ctx context.Context, workspaceID string) ([]SessionSummary, error) {
	return nil, nil
}

func (s *fakeStore) GetSession(ctx context.Context, id string) (*SessionDetail, error) {
	return nil, nil
}

func (s *fakeStore) ListEvents(ctx context.Context, sessionID string) ([]Event, error) {
	return nil, nil
}

func (s *fakeStore) DeleteSession(_ context.Context, _ string) error { return nil }
func (s *fakeStore) DeleteOldEvents(_ context.Context, _ time.Time) (int64, error) {
	return 0, nil
}
func (s *fakeStore) DeleteOrphanedSessions(_ context.Context, _ time.Time) (int64, error) {
	return 0, nil
}
func (s *fakeStore) ListActiveAgents(_ context.Context) ([]ActiveAgent, error) { return nil, nil }

func TestServiceReportCreatesImplicitSession(t *testing.T) {
	store := &fakeStore{}
	svc := NewService(store)

	progress := 42
	result, err := svc.Report(context.Background(), ReportInput{
		AgentID:   "codex-1",
		AgentType: "codex",
		Workspace: "/repo",
		Status:    StatusRunning,
		Progress:  &progress,
		Message:   "working",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.SessionID == "" {
		t.Fatal("expected generated session id")
	}
	if result.EventID == "" {
		t.Fatal("expected generated event id")
	}
	if store.report.SessionID != result.SessionID {
		t.Fatalf("stored report session id mismatch: %q != %q", store.report.SessionID, result.SessionID)
	}
	if store.report.Progress == nil || *store.report.Progress != progress {
		t.Fatalf("expected progress %d, got %#v", progress, store.report.Progress)
	}
}

func TestServiceReportRejectsInvalidStatus(t *testing.T) {
	svc := NewService(&fakeStore{})
	_, err := svc.Report(context.Background(), ReportInput{
		AgentID:   "agent",
		AgentType: "codex",
		Workspace: "/repo",
		Status:    "unknown",
	})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput, got %v", err)
	}
}

func TestServiceReportRejectsHealthTickerStatuses(t *testing.T) {
	svc := NewService(&fakeStore{})
	for _, status := range []Status{StatusStuck, StatusOrphaned} {
		_, err := svc.Report(context.Background(), ReportInput{
			AgentID:   "agent",
			AgentType: "codex",
			Workspace: "/repo",
			Status:    status,
		})
		if !errors.Is(err, ErrInvalidInput) {
			t.Errorf("status %q: expected ErrInvalidInput, got %v", status, err)
		}
	}
}

func TestServiceReportRejectsInvalidProgress(t *testing.T) {
	svc := NewService(&fakeStore{})
	progress := 101
	_, err := svc.Report(context.Background(), ReportInput{
		AgentID:   "agent",
		AgentType: "codex",
		Workspace: "/repo",
		Status:    StatusRunning,
		Progress:  &progress,
	})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput, got %v", err)
	}
}
