package inbox

import (
	"errors"
	"time"
)

// Status is an inbox item's lifecycle state (docs/design/inbox.md):
//
//	open         captured, unprocessed
//	in_progress  claimed by an agent, being enriched
//	converted    linked to a plan (plan_id set); completes with the plan
//	done         the linked plan completed (terminal)
//	discarded    rejected / won't do (terminal, retention-cleaned)
type Status string

const (
	StatusOpen       Status = "open"
	StatusInProgress Status = "in_progress"
	StatusConverted  Status = "converted"
	StatusDone       Status = "done"
	StatusDiscarded  Status = "discarded"
)

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
	ErrInvalidInput     = errors.New("invalid inbox input")
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
