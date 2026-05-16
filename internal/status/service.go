package status

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

var (
	ErrInvalidInput = errors.New("invalid status report")
	ErrNotFound     = errors.New("not found")
)

type Store interface {
	RecordReport(ctx context.Context, report Event, sessionTitle string) error
	ListSessions(ctx context.Context, workspaceID string) ([]SessionSummary, error)
	GetSession(ctx context.Context, id string) (*SessionDetail, error)
	ListEvents(ctx context.Context, sessionID string) ([]Event, error)
	DeleteSession(ctx context.Context, id string) error
	DeleteOldEvents(ctx context.Context, olderThan time.Time) (int64, error)
	DeleteOrphanedSessions(ctx context.Context, olderThan time.Time) (int64, error)
}

type Service struct {
	store Store
	now   func() time.Time
}

func NewService(store Store) *Service {
	return &Service{
		store: store,
		now:   time.Now,
	}
}

func (s *Service) Report(ctx context.Context, input ReportInput) (*ReportResult, error) {
	normalized, err := normalizeReport(input)
	if err != nil {
		return nil, err
	}

	if normalized.SessionID == "" {
		normalized.SessionID = generateID()
	}

	eventID := generateID()
	now := s.now().UTC()
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

	if err := s.store.RecordReport(ctx, event, sessionTitle(normalized)); err != nil {
		return nil, err
	}

	return &ReportResult{SessionID: normalized.SessionID, EventID: eventID}, nil
}

func (s *Service) ListSessions(ctx context.Context, workspaceID string) ([]SessionSummary, error) {
	return s.store.ListSessions(ctx, workspaceID)
}

func (s *Service) GetSession(ctx context.Context, id string) (*SessionDetail, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, fmt.Errorf("%w: session_id is required", ErrInvalidInput)
	}
	return s.store.GetSession(ctx, id)
}

func (s *Service) ListEvents(ctx context.Context, sessionID string) ([]Event, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil, fmt.Errorf("%w: session_id is required", ErrInvalidInput)
	}
	return s.store.ListEvents(ctx, sessionID)
}

func (s *Service) DeleteSession(ctx context.Context, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("%w: session_id is required", ErrInvalidInput)
	}
	return s.store.DeleteSession(ctx, id)
}

func normalizeReport(input ReportInput) (ReportInput, error) {
	input.SessionID = strings.TrimSpace(input.SessionID)
	input.AgentID = strings.TrimSpace(input.AgentID)
	input.AgentType = strings.TrimSpace(input.AgentType)
	input.Workspace = strings.TrimSpace(input.Workspace)
	input.Message = strings.TrimSpace(input.Message)
	input.Status = Status(strings.TrimSpace(string(input.Status)))
	input.GitBranch = strings.TrimSpace(input.GitBranch)

	if input.AgentID == "" {
		return input, fmt.Errorf("%w: agent_id is required", ErrInvalidInput)
	}
	if input.AgentType == "" {
		return input, fmt.Errorf("%w: agent_type is required", ErrInvalidInput)
	}
	if input.Workspace == "" {
		return input, fmt.Errorf("%w: workspace is required", ErrInvalidInput)
	}
	if !validStatus(input.Status) {
		return input, fmt.Errorf("%w: unsupported status %q", ErrInvalidInput, input.Status)
	}
	if input.Progress != nil && (*input.Progress < 0 || *input.Progress > 100) {
		return input, fmt.Errorf("%w: progress must be between 0 and 100", ErrInvalidInput)
	}
	if input.StepCurrent != nil && *input.StepCurrent < 0 {
		return input, fmt.Errorf("%w: step_current must be zero or greater", ErrInvalidInput)
	}
	if input.StepTotal != nil && *input.StepTotal < 0 {
		return input, fmt.Errorf("%w: step_total must be zero or greater", ErrInvalidInput)
	}
	if input.StepCurrent != nil && input.StepTotal != nil && *input.StepTotal > 0 && *input.StepCurrent > *input.StepTotal {
		return input, fmt.Errorf("%w: step_current cannot exceed step_total", ErrInvalidInput)
	}
	if input.Metadata == nil {
		input.Metadata = map[string]any{}
	}
	return input, nil
}

func validStatus(status Status) bool {
	switch status {
	case StatusPending, StatusThinking, StatusPlanning, StatusRunning, StatusEditing, StatusTesting,
		StatusWaiting, StatusBlocked, StatusPaused, StatusHandoff, StatusSucceeded, StatusFailed, StatusCancelled:
		return true
	default:
		return false
	}
}

func sessionTitle(input ReportInput) string {
	if input.Workspace != "" {
		return input.Workspace
	}
	return input.SessionID
}

func generateID() string {
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.NewString()
	}
	return id.String()
}
