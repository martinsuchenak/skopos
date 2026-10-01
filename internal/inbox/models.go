package inbox

import (
	"errors"
	"time"
)

// Status is an inbox item's lifecycle state. The manual path (docs/design/
// inbox.md) uses open/in_progress/converted/done/discarded; the agent
// workflow (docs/design/agent-pipeline.md §1) adds the machine phases:
//
//	open              captured, unprocessed
//	in_progress       claimed by an agent, being enriched
//	queued            Martin queued it for agent work (human-only action)
//	planning          a planner run owns it (system transition)
//	awaiting_approval the plan revision waits for Martin
//	approved          the revision is approved and locked (system continues)
//	implementing      a worker implements the approved revision
//	in_review         branch pushed; Martin reviews the head commit
//	converted         linked to a plan (plan_id set); completes with the plan
//	done              the linked plan completed / Martin marked it done (terminal)
//	discarded         rejected / won't do (terminal, retention-cleaned)
//	failed, blocked   terminal-ish run outcomes with a reason; both return
//	                  to their phase on retry or discard on reject
type Status string

const (
	StatusOpen       Status = "open"
	StatusInProgress Status = "in_progress"
	// Workflow statuses (agent-pipeline §1). System transitions
	// (planning, implementing, in_review, failed, blocked) happen only as a
	// side effect of runs — never from the human actions.
	StatusQueued           Status = "queued"
	StatusPlanning         Status = "planning"
	StatusAwaitingApproval Status = "awaiting_approval"
	StatusApproved         Status = "approved"
	StatusImplementing     Status = "implementing"
	StatusInReview         Status = "in_review"
	StatusFailed           Status = "failed"
	StatusBlocked          Status = "blocked"
	StatusConverted        Status = "converted"
	StatusDone             Status = "done"
	StatusDiscarded        Status = "discarded"
)

// Transition classes: who may perform a status change. Human actions
// additionally require the approver permission; system transitions are the
// executor's (runs in 1b, agent-trial in 1a) and record a reason.
type TransitionActor string

const (
	ActorHuman  TransitionActor = "human"
	ActorSystem TransitionActor = "system"
)

// workflowEdges is the frozen transition matrix (docs/design/agent-pipeline.md
// §1, diagram "Item workflow statuses"). failed shares blocked's edges.
var workflowEdges = map[Status]map[Status]TransitionActor{
	StatusOpen:       {StatusQueued: ActorHuman},
	StatusInProgress: {StatusQueued: ActorHuman}, // queue implies release (review fix 11)
	StatusQueued:     {StatusPlanning: ActorSystem, StatusDiscarded: ActorHuman},
	// Planning runs block too (review fix 5): a planner needing clarification
	// pauses exactly like an implementer.
	StatusPlanning:         {StatusAwaitingApproval: ActorSystem, StatusBlocked: ActorSystem, StatusFailed: ActorSystem, StatusDiscarded: ActorHuman},
	StatusAwaitingApproval: {StatusApproved: ActorHuman, StatusPlanning: ActorHuman, StatusDiscarded: ActorHuman},
	StatusApproved:         {StatusImplementing: ActorSystem, StatusDiscarded: ActorHuman},
	StatusImplementing:     {StatusAwaitingApproval: ActorSystem, StatusInReview: ActorSystem, StatusBlocked: ActorSystem, StatusFailed: ActorSystem, StatusDiscarded: ActorHuman},
	// Retry (with an answer) is Martin's action — the answer feeds the next
	// run's prompt. The target is the phase the item was blocked from:
	// implementing when a revision is locked, planning otherwise (Retry).
	StatusBlocked:  {StatusImplementing: ActorHuman, StatusPlanning: ActorHuman, StatusDiscarded: ActorHuman},
	StatusFailed:   {StatusImplementing: ActorHuman, StatusPlanning: ActorHuman, StatusDiscarded: ActorHuman},
	StatusInReview: {StatusImplementing: ActorHuman, StatusDone: ActorHuman, StatusDiscarded: ActorHuman},
}

// WorkflowTransition reports who may move an item from one workflow status
// to another, and whether the edge exists at all (ok false = illegal move).
// Non-workflow statuses (open, in_progress, converted, done, discarded) have
// no workflow edges; their transitions stay on the manual path (claim,
// convert, discard, …) and are not governed by this matrix.
func WorkflowTransition(from, to Status) (actor TransitionActor, ok bool) {
	edges, exists := workflowEdges[from]
	if !exists {
		return "", false
	}
	actor, ok = edges[to]
	return actor, ok
}

// IsWorkflowStatus reports whether s is one of the agent-pipeline phases.
func IsWorkflowStatus(s Status) bool {
	switch s {
	case StatusQueued, StatusPlanning, StatusAwaitingApproval, StatusApproved,
		StatusImplementing, StatusInReview, StatusFailed, StatusBlocked:
		return true
	}
	return false
}

type Item struct {
	ID               string    `json:"id"`
	WorkspaceID      string    `json:"workspace_id,omitempty"`
	Title            string    `json:"title"`
	Content          string    `json:"content,omitempty"`
	Tags             []string  `json:"tags"`
	Status           Status    `json:"status"`
	Priority         *int      `json:"priority,omitempty"`
	ClaimedByAgentID string    `json:"claimed_by_agent_id,omitempty"`
	AuthorAgentID    string    `json:"author_agent_id"`
	PlanID           string    `json:"plan_id,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`

	// Read-path hydration. List rows carry only Excerpt (content is dropped);
	// GetItem fills ContentHTML (dashboard-safe rendering) and, when the item
	// is converted, the linked Plan summary.
	ContentHTML string       `json:"content_html,omitempty"`
	Excerpt     string       `json:"excerpt,omitempty"`
	Plan        *PlanSummary `json:"plan,omitempty"`
}

// PlanSummary is the linked plan shown on converted items (LEFT JOIN on read;
// nil when the plan row no longer exists).
type PlanSummary struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

var (
	ErrInvalidInput = errors.New("invalid inbox input")
	// ErrStaleRevision mirrors plans.ErrStaleRevision for the inbox surface
	// (approve naming content that changed since Martin saw it) — handlers
	// map it to 409 so clients re-show the plan.
	ErrStaleRevision    = errors.New("revision is stale: a newer revision exists or the plan changed since the snapshot")
	ErrNotFound         = errors.New("not found")
	ErrClaimConflict    = errors.New("item already claimed by another agent")
	ErrAlreadyConverted = errors.New("item already converted to a plan")
	ErrFrozen           = errors.New("item is no longer editable")
)

// CreateInput captures a new item. WorkspaceID may be empty for root
// principals: the item is captured UNFILED (visible to root only) until it
// is filed — rough ideas legitimately precede the where-does-this-belong
// decision. Scoped keys must name a workspace (an unfiled item would be
// invisible to its own creator).
type CreateInput struct {
	WorkspaceID   string   `json:"workspace_id"`
	Title         string   `json:"title"`
	Content       string   `json:"content,omitempty"`
	Tags          []string `json:"tags,omitempty"`
	Priority      int      `json:"priority,omitempty"`
	AuthorAgentID string   `json:"author_agent_id"`
}

// UpdateInput is the enrichment patch. Tags is a pointer so an explicit empty
// array clears the tags while an absent field leaves them unchanged. Priority
// follows the same shape: nil = unchanged, 0 = clear (back to unordered),
// >= 1 = set. A non-empty WorkspaceID files (or re-files) the item while it
// is still editable.
type UpdateInput struct {
	Title       string    `json:"title,omitempty"`
	Content     string    `json:"content,omitempty"`
	Tags        *[]string `json:"tags,omitempty"`
	Priority    *int      `json:"priority,omitempty"`
	WorkspaceID string    `json:"workspace_id,omitempty"`
}

// ConvertInput links an existing plan (created separately via the plan tools)
// to the item and moves it to converted.
type ConvertInput struct {
	PlanID string `json:"plan_id"`
}

// ReorderInput sets an explicit order: the listed ids receive priorities
// 1..N (the board's drag-and-drop renumbering). Items absent from the list
// keep their priority; the caller clears unprioritized ones explicitly.
type ReorderInput struct {
	IDs []string `json:"ids"`
}
