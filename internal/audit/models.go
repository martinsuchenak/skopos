package audit

import (
	"errors"
	"time"
)

var (
	ErrInvalidInput = errors.New("invalid audit input")
)

// Entry is one append-only audit record: what happened, to what, who did it,
// and through which surface (docs/design/agent-pipeline.md §1, plan 01a0bf7c).
type Entry struct {
	ID          string    `json:"id"`
	WorkspaceID string    `json:"workspace_id,omitempty"` // empty for server-level events
	EntityType  string    `json:"entity_type"`            // e.g. inbox_item, api_key, workspace_group
	EntityID    string    `json:"entity_id"`
	Action      string    `json:"action"` // dot-namespaced domain.verb, e.g. inbox.queue
	Actor       string    `json:"actor"`  // rendered at write time: root, key <name>, agent <id>, system, migration
	ActorKeyID  string    `json:"actor_key_id,omitempty"`
	Via         string    `json:"via,omitempty"` // dashboard, slack, cli, mcp, worker, migration, system
	Notes       string    `json:"notes,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

// RecordInput is the caller-facing write shape. Actor fields are derived
// from the request principal inside Record — callers never fabricate them.
type RecordInput struct {
	WorkspaceID string
	EntityType  string
	EntityID    string
	Action      string
	Via         string
	Notes       string
	// AgentID names an acting agent for agent-driven mutations (rendered as
	// "agent <id>"); empty means the principal alone decides the actor.
	AgentID string
}

// Via values. The audit log is write-path only; these label the surface the
// mutation arrived through.
const (
	ViaDashboard = "dashboard"
	ViaSlack     = "slack"
	ViaCLI       = "cli"
	ViaMCP       = "mcp"
	ViaWorker    = "worker"
	ViaMigration = "migration"
	ViaSystem    = "system"
)
