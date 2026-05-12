package blackboard

import (
	"errors"
	"time"
)

type Scope string
type EntryType string

const (
	ScopeSession Scope = "session"
	ScopeBranch  Scope = "branch"
	ScopeProject Scope = "project"
)

const (
	TypeFinding  EntryType = "finding"
	TypeDecision EntryType = "decision"
	TypeBug      EntryType = "bug"
	TypeDebt     EntryType = "debt"
	TypeWarning  EntryType = "warning"
	TypeContext  EntryType = "context"
)

type Entry struct {
	ID            string    `json:"id"`
	Scope         Scope     `json:"scope"`
	BranchName    string    `json:"branch_name,omitempty"`
	SessionID     string    `json:"session_id,omitempty"`
	EntryType     EntryType `json:"entry_type"`
	Title         string    `json:"title"`
	Content       string    `json:"content"`
	CodeRef       string    `json:"code_ref,omitempty"`
	AuthorAgentID string    `json:"author_agent_id"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

var (
	ErrInvalidInput      = errors.New("invalid blackboard input")
	ErrNotFound          = errors.New("not found")
	ErrAlreadyAtTopScope = errors.New("entry is already at project scope")
)

type WriteInput struct {
	Scope         Scope     `json:"scope"`
	BranchName    string    `json:"branch_name,omitempty"`
	SessionID     string    `json:"session_id,omitempty"`
	EntryType     EntryType `json:"entry_type"`
	Title         string    `json:"title"`
	Content       string    `json:"content,omitempty"`
	CodeRef       string    `json:"code_ref,omitempty"`
	AuthorAgentID string    `json:"author_agent_id"`
}

type WriteResult struct {
	ID    string `json:"id"`
	Scope Scope  `json:"scope"`
}

type Bundle struct {
	Entries        []Entry `json:"entries"`
	MarkdownBundle string  `json:"markdown_bundle"`
}
