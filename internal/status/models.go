package status

import "time"

type Status string

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
)

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
}

type ReportResult struct {
	SessionID string `json:"session_id"`
	EventID   string `json:"event_id"`
}

type SessionSummary struct {
	ID         string       `json:"id"`
	Title      string       `json:"title"`
	Workspace  string       `json:"workspace"`
	Status     Status       `json:"status"`
	AgentCount int          `json:"agent_count"`
	StartedAt  time.Time    `json:"started_at"`
	UpdatedAt  time.Time    `json:"updated_at"`
	Agents     []AgentState `json:"agents,omitempty"`
}

type SessionDetail struct {
	SessionSummary
	Events []Event `json:"events,omitempty"`
}

type Agent struct {
	ID          string    `json:"id"`
	Type        string    `json:"type"`
	Workspace   string    `json:"workspace"`
	FirstSeenAt time.Time `json:"first_seen_at"`
	LastSeenAt  time.Time `json:"last_seen_at"`
}

type AgentState struct {
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
	UpdatedAt   time.Time      `json:"updated_at"`
}

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
}
