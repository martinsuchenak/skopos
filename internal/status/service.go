package status

import (
	"sort"

	"github.com/martinsuchenak/skopos/internal/auth"
	"github.com/martinsuchenak/skopos/internal/events"
	"context"
	"errors"
	"fmt"
	"github.com/martinsuchenak/skopos/internal/ids"
	"strings"
	"time"
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
	ListActiveAgents(ctx context.Context) ([]ActiveAgent, error)
}

type Service struct {
	store      Store
	now        func() time.Time
	registerWS func(id string)
	publisher  events.Publisher
}

func NewService(store Store) *Service {
	return &Service{
		store: store,
		now:   time.Now,
	}
}

// SetPublisher installs the event bus; mutations publish with their
// authoritative workspace. Nil (the default) disables publishing.
func (s *Service) SetPublisher(p events.Publisher) { s.publisher = p }

// SetWorkspaceRegistrar installs a callback invoked with the workspace id of
// every accepted report — session-derived workspaces are auto-registered so
// they persist in the registry.
func (s *Service) SetWorkspaceRegistrar(fn func(id string)) { s.registerWS = fn }

func (s *Service) Report(ctx context.Context, input ReportInput) (*ReportResult, error) {
	normalized, err := normalizeReport(input)
	if err != nil {
		return nil, err
	}

	if normalized.SessionID == "" {
		normalized.SessionID = ids.New()
	} else if existing, err := s.store.GetSession(ctx, normalized.SessionID); err == nil {
		// Attaching to an existing session: authorize against the session's
		// ACTUAL workspace, never the client-declared field — and the
		// binding is immutable, so the declared workspace cannot rewrite it.
		if err := auth.RequireWorkspaceQuiet(ctx, existing.Workspace); err != nil {
			// Uniform with a nonexistent id: no existence or ownership oracle.
			return nil, fmt.Errorf("%w: session %s", ErrNotFound, normalized.SessionID)
		}
		normalized.Workspace = existing.Workspace
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}

	eventID := ids.New()
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

	// Scope enforcement + server-stamped provenance: the resolved key is
	// recorded by the server, giving an audit counterpart to the
	// client-asserted author_agent_id.
	if err := auth.RequireWorkspace(ctx, normalized.Workspace); err != nil {
		return nil, err
	}
	if p := auth.PrincipalFromContext(ctx); p != nil && !p.Root {
		if normalized.Metadata == nil {
			normalized.Metadata = map[string]any{}
		}
		normalized.Metadata["auth_key"] = map[string]any{"id": p.KeyID, "name": p.Name}
		event.Metadata = normalized.Metadata
	}

	if err := s.store.RecordReport(ctx, event, sessionTitle(normalized)); err != nil {
		return nil, err
	}
	if s.publisher != nil {
		s.publisher.Publish(events.Event{Type: events.TypeSessions, Workspace: normalized.Workspace})
	}

	// Best-effort registry upsert; a failure must not fail the report.
	if s.registerWS != nil {
		s.registerWS(normalized.Workspace)
	}

	return &ReportResult{SessionID: normalized.SessionID, EventID: eventID}, nil
}

func (s *Service) ListSessions(ctx context.Context, workspaceID string) ([]SessionSummary, error) {
	if workspaceID != "" {
		if err := auth.RequireWorkspace(ctx, workspaceID); err != nil {
			return nil, err
		}
		return s.store.ListSessions(ctx, workspaceID)
	}
	if !auth.ScopedContext(ctx) {
		return s.store.ListSessions(ctx, workspaceID)
	}
	var merged []SessionSummary
	seen := map[string]bool{}
	for _, ws := range auth.PrincipalFromContext(ctx).WorkspaceList() {
		sessions, err := s.store.ListSessions(ctx, ws)
		if err != nil {
			return nil, err
		}
		for _, sess := range sessions {
			if !seen[sess.ID] {
				seen[sess.ID] = true
				merged = append(merged, sess)
			}
		}
	}
	sort.SliceStable(merged, func(i, j int) bool { return merged[i].UpdatedAt.After(merged[j].UpdatedAt) })
	return merged, nil
}

func (s *Service) GetSession(ctx context.Context, id string) (*SessionDetail, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, fmt.Errorf("%w: session_id is required", ErrInvalidInput)
	}
	session, err := s.store.GetSession(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := auth.RequireWorkspaceQuiet(ctx, session.Workspace); err != nil {
		return nil, fmt.Errorf("%w: session %s", ErrNotFound, id)
	}
	return session, nil
}

func (s *Service) ListEvents(ctx context.Context, sessionID string) ([]Event, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil, fmt.Errorf("%w: session_id is required", ErrInvalidInput)
	}
	// Authorize against the session's workspace; unknown ids report
	// not-found first so existence is not leaked across tenants.
	session, err := s.store.GetSession(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if err := auth.RequireWorkspaceQuiet(ctx, session.Workspace); err != nil {
		return nil, fmt.Errorf("%w: session %s", ErrNotFound, sessionID)
	}
	return s.store.ListEvents(ctx, sessionID)
}

func (s *Service) ListActiveAgents(ctx context.Context) ([]ActiveAgent, error) {
	agents, err := s.store.ListActiveAgents(ctx)
	if err != nil {
		return nil, err
	}
	if !auth.ScopedContext(ctx) {
		return agents, nil
	}
	p := auth.PrincipalFromContext(ctx)
	out := make([]ActiveAgent, 0, len(agents))
	for _, a := range agents {
		if p.CanAccess(a.Workspace) {
			out = append(out, a)
		}
	}
	return out, nil
}
func (s *Service) DeleteSession(ctx context.Context, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("%w: session_id is required", ErrInvalidInput)
	}
	// Authorize before deleting; unknown ids report not-found first so
	// existence is not leaked across tenants.
	session, err := s.store.GetSession(ctx, id)
	if err != nil {
		return err
	}
	if err := auth.RequireWorkspaceQuiet(ctx, session.Workspace); err != nil {
		return fmt.Errorf("%w: session %s", ErrNotFound, id)
	}
	if err := s.store.DeleteSession(ctx, id); err != nil {
		return err
	}
	if s.publisher != nil {
		s.publisher.Publish(events.Event{Type: events.TypeSessions, Workspace: session.Workspace})
	}
	return nil
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
		return input, fmt.Errorf("%w: unsupported status %q. Valid statuses: pending, thinking, planning, running, editing, testing, waiting, blocked, paused, handoff, succeeded, failed, cancelled", ErrInvalidInput, input.Status)
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
