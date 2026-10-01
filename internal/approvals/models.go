package approvals

import "time"

// Gates: which checkpoint an approval names (docs/design/agent-pipeline.md §4).
const (
	GatePlan   = "plan"   // subject: "<revision_id>+<base_sha>"
	GateReview = "review" // subject: "<head_sha>"
)

// Decisions.
const (
	DecisionApproved         = "approved"
	DecisionRejected         = "rejected"
	DecisionChangesRequested = "changes_requested"
)

// Entry records that a human decision named an immutable subject: a plan
// revision plus its base commit, or a branch head. Approvals are append-only;
// a new commit after a review approval makes that review stale (the subject
// no longer matches the head), exactly like GitHub stale-review dismissal.
type Entry struct {
	ID          string    `json:"id"`
	WorkspaceID string    `json:"workspace_id"`
	ItemID      string    `json:"item_id"`
	Gate        string    `json:"gate"`
	Subject     string    `json:"subject"`
	Decision    string    `json:"decision"`
	Actor       string    `json:"actor"`
	ActorKeyID  string    `json:"actor_key_id,omitempty"`
	Via         string    `json:"via,omitempty"`
	Notes       string    `json:"notes,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

// RecordInput is the caller-facing write shape; the actor is derived from
// the request principal inside Record, never trusted from the caller.
type RecordInput struct {
	WorkspaceID string
	ItemID      string
	Gate        string
	Subject     string
	Decision    string
	Via         string
	Notes       string
}
