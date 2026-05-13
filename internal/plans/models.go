package plans

import (
	"errors"
	"time"
)

type PlanStatus string
type ItemStatus string

const (
	PlanActive    PlanStatus = "active"
	PlanCompleted PlanStatus = "completed"
	PlanArchived  PlanStatus = "archived"
)

const (
	ItemPending    ItemStatus = "pending"
	ItemInProgress ItemStatus = "in_progress"
	ItemDone       ItemStatus = "done"
	ItemBlocked    ItemStatus = "blocked"
)

type Plan struct {
	ID            string     `json:"id"`
	Name          string     `json:"name"`
	BranchName    string     `json:"branch_name,omitempty"` // nullable in DB; storage scans via sql.NullString
	Description   string     `json:"description,omitempty"`
	Status        PlanStatus `json:"status"`
	AuthorAgentID string     `json:"author_agent_id"`
	Items         []Item     `json:"items,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

type Item struct {
	ID               string     `json:"id"`
	PlanID           string     `json:"plan_id"`
	Title            string     `json:"title"`
	Description      string     `json:"description,omitempty"`
	Status           ItemStatus `json:"status"`
	Position         int        `json:"position"`
	ClaimedByAgentID string     `json:"claimed_by_agent_id,omitempty"` // nullable in DB; storage scans via sql.NullString
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
}

var (
	ErrInvalidInput = errors.New("invalid plans input")
	ErrNotFound     = errors.New("not found")
)

type CreatePlanInput struct {
	Name          string `json:"name"`
	BranchName    string `json:"branch_name,omitempty"`
	Description   string `json:"description,omitempty"`
	AuthorAgentID string `json:"author_agent_id"`
}

type UpdatePlanInput struct {
	Name        string     `json:"name,omitempty"`
	Description string     `json:"description,omitempty"`
	Status      PlanStatus `json:"status,omitempty"`
}

type CreateItemInput struct {
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	Position    *int   `json:"position,omitempty"`
}

// UpdateItemInput: ClaimedByAgentID nil = don't change, *"" = release claim, *"id" = claim.
type UpdateItemInput struct {
	Status           ItemStatus `json:"status,omitempty"`
	ClaimedByAgentID *string    `json:"claimed_by_agent_id"`
}
