package inbox

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/martinsuchenak/skopos/internal/qa"
)

// Status is an inbox item's lifecycle state. The manual path (docs/design/
// inbox.md) uses open/in_progress/converted/done/discarded; the agent
// workflow (docs/design/agent-pipeline.md §1) adds the machine phases:
//
//	open              captured, unprocessed
//	in_progress       claimed by an agent, being enriched
//	queued            the approver queued it for agent work (human-only action)
//	planning          a planner run owns it (system transition)
//	awaiting_approval the plan revision waits for the approver
//	approved          the revision is approved and locked (system continues)
//	implementing      a worker implements the approved revision
//	in_review         branch pushed; the approver reviews the head commit
//	converted         linked to a plan (plan_id set); completes with the plan
//	done              the linked plan completed / the approver marked it done (terminal)
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
	// StatusNeedsYou is a list filter only, never an item's status: the
	// phases that wait for an approver (plan gate, sign-off, blocked/failed).
	StatusNeedsYou Status = "needs_you"
)

// Transition classes: who may perform a status change. Human actions
// additionally require the approver permission; system transitions are the
// executor's (runs) and record a reason.
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
	// queued → implementing: a review-only item's branch_review run is
	// claimed (tmp/branch-handover — design.md §4). Review-only items only
	// (reviewOnlyEdge), like in_review → planning below.
	StatusQueued: {StatusPlanning: ActorSystem, StatusImplementing: ActorSystem, StatusDiscarded: ActorHuman},
	// Planning runs block too (review fix 5): a planner needing clarification
	// pauses exactly like an implementer.
	StatusPlanning:         {StatusAwaitingApproval: ActorSystem, StatusBlocked: ActorSystem, StatusFailed: ActorSystem, StatusDiscarded: ActorHuman},
	StatusAwaitingApproval: {StatusApproved: ActorHuman, StatusPlanning: ActorHuman, StatusDiscarded: ActorHuman},
	StatusApproved:         {StatusImplementing: ActorSystem, StatusDiscarded: ActorHuman},
	StatusImplementing:     {StatusAwaitingApproval: ActorSystem, StatusInReview: ActorSystem, StatusBlocked: ActorSystem, StatusFailed: ActorSystem, StatusDiscarded: ActorHuman},
	// Retry (with an answer) is the approver's action — the answer feeds the next
	// run's prompt. The target is the phase the item was blocked from:
	// implementing when a revision is locked, planning otherwise (Retry).
	StatusBlocked: {StatusImplementing: ActorHuman, StatusPlanning: ActorHuman, StatusDiscarded: ActorHuman},
	StatusFailed:  {StatusImplementing: ActorHuman, StatusPlanning: ActorHuman, StatusDiscarded: ActorHuman},
	// in_review → planning: changes requested on a review-only item, which
	// has no plan yet — the handover is planned first.
	StatusInReview: {StatusImplementing: ActorHuman, StatusPlanning: ActorHuman, StatusDone: ActorHuman, StatusDiscarded: ActorHuman},
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
	ID          string   `json:"id"`
	WorkspaceID string   `json:"workspace_id,omitempty"`
	Title       string   `json:"title"`
	Content     string   `json:"content,omitempty"`
	Tags        []string `json:"tags"`
	Status      Status   `json:"status"`
	Priority    *int     `json:"priority,omitempty"`
	// SlackThreadTS is the relay's notification thread root for this item
	// (empty = no thread yet). Persisted so a server restart re-binds the
	// thread instead of stranding replies in a dead one.
	SlackThreadTS    string `json:"slack_thread_ts,omitempty"`
	ClaimedByAgentID string `json:"claimed_by_agent_id,omitempty"`
	AuthorAgentID    string `json:"author_agent_id"`
	PlanID           string `json:"plan_id,omitempty"`
	// Agents is the item's multi-agent switch for stage groups: "panel"
	// (the repository's configured agents), "solo" (one agent), or empty
	// (the repository default, --agents-default).
	Agents string `json:"agents,omitempty"`
	// Branch is the branch the item works on, named by the approver: an existing
	// one is taken over (handover, review), a new one is created under this
	// name. Empty = the worker names it (branch_template).
	Branch string `json:"branch,omitempty"`
	// Ref is the item's external reference (a ticket or job number), the
	// {ref} of a repository's branch template.
	Ref string `json:"ref,omitempty"`
	// ExternalURL links the item back to the job it came from in another
	// system (https only).
	ExternalURL string `json:"external_url,omitempty"`
	// Flow is "review" for a review-only item (the agent reviews an
	// existing branch; nothing is planned or implemented), "" otherwise.
	Flow      string    `json:"flow,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// Read-path hydration. List rows carry only Excerpt (content is dropped);
	// GetItem fills ContentHTML (dashboard-safe rendering) and, when the item
	// is converted, the linked Plan summary.
	ContentHTML string       `json:"content_html,omitempty"`
	Excerpt     string       `json:"excerpt,omitempty"`
	Plan        *PlanSummary `json:"plan,omitempty"`
	// ContentParts is the content cut for the dashboard's detail view:
	// The author's text, then the pipeline's sections in rounds. Filled by
	// GET /api/inbox/{id}?parts=1 only.
	ContentParts *ContentParts `json:"content_parts,omitempty"`
	// AnswersMatched: on an action's response, what its answers were read
	// as (internal/qa Match): linked to a question, or a general answer.
	AnswersMatched []qa.Match `json:"answers_matched,omitempty"`
	// StartError: on a create with start, why the item was created but
	// not started (it stays open).
	StartError string `json:"start_error,omitempty"`
}

// PlanSummary is the linked plan shown on converted items (LEFT JOIN on read;
// nil when the plan row no longer exists).
type PlanSummary struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

// Per-item multi-agent switch values (Item.Agents).
const (
	AgentsPanel = "panel"
	AgentsSolo  = "solo"
)

// FlowReview marks a review-only item (Item.Flow).
const FlowReview = "review"

var (
	reBranchName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)
	reRef        = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
)

// NormalizeBranch validates a branch name the way git and the worker's
// push guard will: letters, digits, '.', '_', '/', '-'; no "..", "//",
// "@{", trailing '/' or '.', and no ".lock" component. Empty = none.
func NormalizeBranch(v string) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", nil
	}
	bad := len(v) > 120 || !reBranchName.MatchString(v) ||
		strings.Contains(v, "..") || strings.Contains(v, "//") ||
		strings.HasSuffix(v, "/") || strings.HasSuffix(v, ".")
	for _, part := range strings.Split(v, "/") {
		if strings.HasSuffix(part, ".lock") || strings.HasPrefix(part, ".") {
			bad = true
		}
	}
	if bad {
		return "", fmt.Errorf("%w: %q is not a usable branch name", ErrInvalidInput, v)
	}
	return v, nil
}

// NormalizeRef validates an external reference (JOB100234): letters,
// digits, '.', '_', '-', at most 40 characters. Empty = none.
func NormalizeRef(v string) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", nil
	}
	if len(v) > 40 || !reRef.MatchString(v) || strings.Contains(v, "..") || strings.HasSuffix(v, ".") {
		return "", fmt.Errorf("%w: %q is not a usable reference (letters, digits, '.', '_', '-')", ErrInvalidInput, v)
	}
	return v, nil
}

// maxExternalURL bounds Item.ExternalURL.
const maxExternalURL = 2000

// NormalizeExternalURL validates a link to the item's job elsewhere: an
// absolute https URL with a host, at most 2000 characters. Empty = none.
func NormalizeExternalURL(v string) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", nil
	}
	u, err := url.Parse(v)
	if err != nil || len(v) > maxExternalURL || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return "", fmt.Errorf("%w: external_url must be an https URL of at most %d characters", ErrInvalidInput, maxExternalURL)
	}
	return v, nil
}

// Start values for CreateInput.Start.
const (
	StartQueue  = "queue"
	StartReview = "review"
)

// NormalizeAgents validates an agents value: "panel", "solo", or empty
// (repository default). "default" is accepted as an alias for empty.
func NormalizeAgents(v string) (string, error) {
	switch v = strings.ToLower(strings.TrimSpace(v)); v {
	case "", "default":
		return "", nil
	case AgentsPanel, AgentsSolo:
		return v, nil
	}
	return "", fmt.Errorf("%w: agents must be panel, solo or default, not %q", ErrInvalidInput, v)
}

var (
	ErrInvalidInput = errors.New("invalid inbox input")
	// ErrStaleRevision mirrors plans.ErrStaleRevision for the inbox surface
	// (approve naming content that changed since the approver saw it) — handlers
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
	// Agents: "panel", "solo", or empty for the repository default.
	Agents string `json:"agents,omitempty"`
	// Branch and Ref: see Item.
	Branch string `json:"branch,omitempty"`
	Ref    string `json:"ref,omitempty"`
	// ExternalURL: see Item.
	ExternalURL string `json:"external_url,omitempty"`
	// Start: "queue" (the pipeline) or "review" (review-only) starts the
	// item right after creating it, as POST /api/inbox/{id}/queue or
	// /review would (approver permission). Empty = the item stays open.
	Start string `json:"start,omitempty"`
	// Via labels the surface for the start's audit entry (the REST
	// handler fills it from X-Skopos-Via).
	Via string `json:"-"`
}

// ListFilter selects items for ListItemsFilter. Empty fields match
// anything; Ref is an exact match.
type ListFilter struct {
	WorkspaceID string
	Status      string
	Tag         string
	Query       string
	Ref         string
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
	// Agents: nil = unchanged, "" = back to the repository default,
	// "panel" / "solo" = set.
	Agents *string `json:"agents,omitempty"`
	// Branch and Ref: nil = unchanged, "" = clear.
	Branch *string `json:"branch,omitempty"`
	Ref    *string `json:"ref,omitempty"`
	// ExternalURL: nil = unchanged, "" = clear.
	ExternalURL *string `json:"external_url,omitempty"`
	// Via labels the surface for the audit log of workflow-item edits
	// (dashboard, slack, cli, mcp); the REST handler defaults it.
	Via string `json:"via,omitempty"`
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
