// Package qa keeps the record of questions and answers: what the planner
// and the implementer asked about an item, and what the approver answered,
// through which surface, with which action (tmp/questions-and-answers —
// design.md). Rows are append-only; the item's "## Questions — …" and
// "## Answers" sections are renderings of them.
package qa

import (
	"errors"
	"time"
)

var (
	ErrInvalidInput = errors.New("invalid input")
	ErrNotFound     = errors.New("not found")
)

// Who asks.
const (
	AskerPlanner     = "planner"
	AskerImplementer = "implementer"
	AskerWorker      = "worker"
)

// What a set was asked about.
const (
	SubjectPlanRevision = "plan_revision" // <revision>+<base_sha>
	SubjectBase         = "base"          // <base_sha>
	SubjectHead         = "head"          // <head_sha>
	SubjectNone         = "none"
)

// Where a set came from.
const (
	SourceWorker   = "worker"
	SourceImported = "imported"
)

// Answer kinds.
const (
	KindAnswer  = "answer"  // to one question: an option and/or text
	KindGeneral = "general" // free text to a set as a whole
	KindSkipped = "skipped" // a gate passed the question unanswered
)

// The action an answer came with.
const (
	ActionApprove        = "approve"
	ActionRequestChanges = "request_changes"
	ActionRetry          = "retry"
	ActionAnswer         = "answer"
	ActionImported       = "imported"
)

// Question states, derived from the rows (State).
const (
	StateOpen             = "open"
	StateAnswered         = "answered"
	StateAnsweredFreeText = "answered_free_text"
	StateSkipped          = "skipped"
	StateSuperseded       = "superseded"
)

// Set is one asking: every question one result returned.
type Set struct {
	ID          string     `json:"id"`
	WorkspaceID string     `json:"workspace_id"`
	ItemID      string     `json:"item_id"`
	Asker       string     `json:"asker"`
	Stage       string     `json:"stage,omitempty"`
	RunID       string     `json:"run_id,omitempty"`
	Runner      string     `json:"runner,omitempty"`
	Label       string     `json:"label,omitempty"`
	SubjectKind string     `json:"subject_kind"`
	Subject     string     `json:"subject,omitempty"`
	Blocking    bool       `json:"blocking"`
	Source      string     `json:"source"`
	CreatedAt   time.Time  `json:"created_at"`
	Questions   []Question `json:"questions"`
	// General: free-text answers to the set as a whole.
	General []Answer `json:"general,omitempty"`
	// Open: the set has a question still waiting for an answer.
	Open bool `json:"open"`
}

// Question is one question of a set; State and Answers are filled on reads.
type Question struct {
	ID      string   `json:"id"`
	SetID   string   `json:"set_id"`
	N       int      `json:"n"`
	Text    string   `json:"text"`
	Options []string `json:"options"`
	State   string   `json:"state,omitempty"`
	Answers []Answer `json:"answers,omitempty"`
}

// Answer is one answer row.
type Answer struct {
	ID          string    `json:"id"`
	WorkspaceID string    `json:"workspace_id"`
	ItemID      string    `json:"item_id"`
	Kind        string    `json:"kind"`
	QuestionID  string    `json:"question_id,omitempty"`
	SetID       string    `json:"set_id,omitempty"`
	Option      string    `json:"option,omitempty"`
	Text        string    `json:"text,omitempty"`
	Replaces    string    `json:"replaces,omitempty"`
	Action      string    `json:"action"`
	ApprovalID  string    `json:"approval_id,omitempty"`
	Actor       string    `json:"actor"`
	ActorKeyID  string    `json:"actor_key_id,omitempty"`
	Via         string    `json:"via,omitempty"`
	UsedByRun   string    `json:"used_by_run,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	// AfterUse: a correction of an answer a run had already used.
	AfterUse bool `json:"after_use,omitempty"`
	// Replaced: a later correction replaces it.
	Replaced bool `json:"replaced,omitempty"`
}

// OptionLetterOf is the letter of the nth option (0-based): a, b, c…
func OptionLetterOf(i int) string {
	if i < 26 {
		return string(rune('a' + i))
	}
	return "?"
}

// Record is an item's questions and answers: the sets in order, and the
// free answers — text given with an action when no question was open (an
// answer to a blocker), recorded as general answers to no set.
type Record struct {
	Sets []Set    `json:"sets"`
	Free []Answer `json:"free,omitempty"`
}

// StageQA is the stage of a QA run: its questions are asked by the worker
// (AskerWorker) on the run's behalf and read as QA's.
const StageQA = "qa"

// Asked names who asked a set, as people read it: a plan candidate's label,
// "the implementer", "QA", or "" for a single planner (or the worker).
func (s Set) Asked() string {
	if s.Label != "" {
		return s.Label
	}
	if s.Asker == AskerImplementer {
		return "the implementer"
	}
	if s.Stage == StageQA {
		return "QA"
	}
	return ""
}

// Heading is the item section a set is rendered under.
func (s Set) Heading() string {
	if s.Asker == AskerImplementer {
		return "Questions — implementer"
	}
	if s.Stage == StageQA {
		return "Questions — QA"
	}
	if s.Label != "" {
		return "Questions — planner " + s.Label
	}
	return "Questions — planner"
}
