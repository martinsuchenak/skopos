package workspaces

import (
	"errors"
	"time"
)

var (
	ErrInvalidInput = errors.New("invalid workspace input")
	ErrNotFound     = errors.New("not found")
)

// Workspace is a registered workspace identifier. The same id is used as the
// `workspace`/`workspace_id` value on sessions, blackboard entries, and plans.
type Workspace struct {
	ID        string    `json:"id"`
	Name      string    `json:"name,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

type CreateInput struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}
