package status

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

type Storage struct {
	db *sql.DB
}

func NewStorage(db *sql.DB) *Storage {
	return &Storage{db: db}
}

func (s *Storage) RecordReport(ctx context.Context, report Event, sessionTitle string) error {
	metadata, err := json.Marshal(report.Metadata)
	if err != nil {
		return fmt.Errorf("marshalling metadata: %w", err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	now := formatTime(report.CreatedAt)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO sessions (id, title, workspace, status, started_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			workspace = excluded.workspace,
			status = excluded.status,
			updated_at = excluded.updated_at
	`, report.SessionID, sessionTitle, report.Workspace, string(report.Status), now, now); err != nil {
		return fmt.Errorf("upserting session: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO agents (id, type, workspace, first_seen_at, last_seen_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			type = excluded.type,
			workspace = excluded.workspace,
			last_seen_at = excluded.last_seen_at
	`, report.AgentID, report.AgentType, report.Workspace, now, now); err != nil {
		return fmt.Errorf("upserting agent: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO agent_states (
			session_id, agent_id, agent_type, workspace, status, progress, step_current,
			step_total, message, snippet, metadata, updated_at
		)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
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
			updated_at = excluded.updated_at
	`, report.SessionID, report.AgentID, report.AgentType, report.Workspace, string(report.Status),
		nullableInt(report.Progress), nullableInt(report.StepCurrent), nullableInt(report.StepTotal),
		report.Message, report.Snippet, string(metadata), now); err != nil {
		return fmt.Errorf("upserting agent state: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO events (
			id, session_id, agent_id, agent_type, workspace, status, progress, step_current,
			step_total, message, snippet, metadata, created_at
		)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, report.ID, report.SessionID, report.AgentID, report.AgentType, report.Workspace, string(report.Status),
		nullableInt(report.Progress), nullableInt(report.StepCurrent), nullableInt(report.StepTotal),
		report.Message, report.Snippet, string(metadata), now); err != nil {
		return fmt.Errorf("inserting event: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

func (s *Storage) ListSessions(ctx context.Context) ([]SessionSummary, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT s.id, s.title, s.workspace, s.status, s.started_at, s.updated_at, COUNT(a.agent_id)
		FROM sessions s
		LEFT JOIN agent_states a ON a.session_id = s.id
		GROUP BY s.id, s.title, s.workspace, s.status, s.started_at, s.updated_at
		ORDER BY s.updated_at DESC
	`)
	if err != nil {
		return nil, fmt.Errorf("listing sessions: %w", err)
	}
	defer rows.Close()

	var sessions []SessionSummary
	for rows.Next() {
		var session SessionSummary
		var startedAt, updatedAt string
		if err := rows.Scan(&session.ID, &session.Title, &session.Workspace, &session.Status, &startedAt, &updatedAt, &session.AgentCount); err != nil {
			return nil, fmt.Errorf("scanning session: %w", err)
		}
		session.StartedAt = parseTime(startedAt)
		session.UpdatedAt = parseTime(updatedAt)
		sessions = append(sessions, session)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating sessions: %w", err)
	}
	return sessions, nil
}

func (s *Storage) GetSession(ctx context.Context, id string) (*SessionDetail, error) {
	var detail SessionDetail
	var startedAt, updatedAt string
	if err := s.db.QueryRowContext(ctx, `
		SELECT id, title, workspace, status, started_at, updated_at
		FROM sessions
		WHERE id = ?
	`, id).Scan(&detail.ID, &detail.Title, &detail.Workspace, &detail.Status, &startedAt, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: session %s", ErrNotFound, id)
		}
		return nil, fmt.Errorf("getting session: %w", err)
	}
	detail.StartedAt = parseTime(startedAt)
	detail.UpdatedAt = parseTime(updatedAt)

	agents, err := s.listAgentStates(ctx, id)
	if err != nil {
		return nil, err
	}
	events, err := s.ListEvents(ctx, id)
	if err != nil {
		return nil, err
	}
	detail.Agents = agents
	detail.AgentCount = len(agents)
	detail.Events = events
	return &detail, nil
}

func (s *Storage) ListEvents(ctx context.Context, sessionID string) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, session_id, agent_id, agent_type, workspace, status, progress, step_current,
			step_total, message, snippet, metadata, created_at
		FROM events
		WHERE session_id = ?
		ORDER BY created_at DESC
	`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("listing events: %w", err)
	}
	defer rows.Close()

	var events []Event
	for rows.Next() {
		event, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating events: %w", err)
	}
	return events, nil
}

func (s *Storage) listAgentStates(ctx context.Context, sessionID string) ([]AgentState, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT session_id, agent_id, agent_type, workspace, status, progress, step_current,
			step_total, message, snippet, metadata, updated_at
		FROM agent_states
		WHERE session_id = ?
		ORDER BY updated_at DESC
	`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("listing agent states: %w", err)
	}
	defer rows.Close()

	var states []AgentState
	for rows.Next() {
		state, err := scanAgentState(rows)
		if err != nil {
			return nil, err
		}
		states = append(states, state)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating agent states: %w", err)
	}
	return states, nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanEvent(row rowScanner) (Event, error) {
	var event Event
	var progress, stepCurrent, stepTotal sql.NullInt64
	var metadata, createdAt string
	if err := row.Scan(&event.ID, &event.SessionID, &event.AgentID, &event.AgentType, &event.Workspace,
		&event.Status, &progress, &stepCurrent, &stepTotal, &event.Message, &event.Snippet, &metadata, &createdAt); err != nil {
		return event, fmt.Errorf("scanning event: %w", err)
	}
	event.Progress = intPtr(progress)
	event.StepCurrent = intPtr(stepCurrent)
	event.StepTotal = intPtr(stepTotal)
	event.Metadata = parseMetadata(metadata)
	event.CreatedAt = parseTime(createdAt)
	return event, nil
}

func scanAgentState(row rowScanner) (AgentState, error) {
	var state AgentState
	var progress, stepCurrent, stepTotal sql.NullInt64
	var metadata, updatedAt string
	if err := row.Scan(&state.SessionID, &state.AgentID, &state.AgentType, &state.Workspace,
		&state.Status, &progress, &stepCurrent, &stepTotal, &state.Message, &state.Snippet, &metadata, &updatedAt); err != nil {
		return state, fmt.Errorf("scanning agent state: %w", err)
	}
	state.Progress = intPtr(progress)
	state.StepCurrent = intPtr(stepCurrent)
	state.StepTotal = intPtr(stepTotal)
	state.Metadata = parseMetadata(metadata)
	state.UpdatedAt = parseTime(updatedAt)
	return state, nil
}

func nullableInt(value *int) any {
	if value == nil {
		return nil
	}
	return *value
}

func intPtr(value sql.NullInt64) *int {
	if !value.Valid {
		return nil
	}
	converted := int(value.Int64)
	return &converted
}

func parseMetadata(raw string) map[string]any {
	var metadata map[string]any
	if err := json.Unmarshal([]byte(raw), &metadata); err != nil || metadata == nil {
		return map[string]any{}
	}
	return metadata
}

func formatTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

func parseTime(raw string) time.Time {
	parsed, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}
	}
	return parsed
}
