package runs

import (
	"errors"
	"time"
)

var (
	ErrInvalidInput = errors.New("invalid runs input")
	ErrNotFound     = errors.New("not found")
	// ErrLeaseHeld is returned when a report or transition arrives for a run
	// whose lease another worker now holds (the previous owner timed out).
	ErrLeaseHeld = errors.New("run lease held by another worker")
	// ErrDuplicateActive is returned when queueing a stage an item already
	// has an active (queued/claimed/running/paused) run for — the inbox
	// hooks treat it as a no-op (idempotent re-queue).
	ErrDuplicateActive = errors.New("item already has an active run for that stage")
)

// Status is a run's lifecycle state.
//
//	queued       waiting for a matching worker
//	claimed      a worker took it (lease running); running is the same lease
//	running      the agent is executing (set by the worker's first heartbeat)
//	paused       amendment or quota: keeps its worker (affinity), resumable
//	succeeded    the stage completed; the queue chains the next stage
//	failed       the stage failed (fail_reason set)
//	interrupted  the lease expired or the worker died; affinity kept — the
//	             owner re-claims with its session, others only via branch
//	cancelled    the item was rejected or done while the run was queued
type Status string

const (
	StatusQueued      Status = "queued"
	StatusClaimed     Status = "claimed"
	StatusRunning     Status = "running"
	StatusPaused      Status = "paused"
	StatusSucceeded   Status = "succeeded"
	StatusFailed      Status = "failed"
	StatusInterrupted Status = "interrupted"
	StatusCancelled   Status = "cancelled"
)

// Stages of the pipeline. The sequence per workspace is configurable; these
// are the names stages may take.
const (
	StagePlan      = "plan"
	StageImplement = "implement"
	StageReview    = "review"
	StageQA        = "qa"
)

// ResumeKind: how a takeover may continue (affinity's escape hatch).
const (
	ResumeSession = "session" // same worker only — the agent session lives there
	ResumeBranch  = "branch"  // any worker, fresh context from the pushed branch
)

// takeoverGrace: an interrupted run becomes branch-resumable after this long
// without its owner reclaiming it (review M1).
const takeoverGrace = 30 * time.Minute

// Run is one stage of one item's lifecycle on the queue.
type Run struct {
	ID          string `json:"id"`
	WorkspaceID string `json:"workspace_id"`
	ItemID      string `json:"item_id"`
	PlanRevID   string `json:"plan_rev_id,omitempty"`
	Stage       string `json:"stage"`
	Runner      string `json:"runner,omitempty"`    // resolved preference at queue time
	WorkerID    string `json:"worker_id,omitempty"` // affinity owner
	// LeaseToken is the fencing token (M5): set ONLY in the claim
	// response, never stored or listed. LeaseTokenHash/ClaimedKeyID are
	// the stored forms.
	LeaseToken     string    `json:"lease_token,omitempty"`
	LeaseTokenHash string    `json:"-"`
	ClaimedKeyID   string    `json:"-"`
	Status         Status    `json:"status"`
	ResumeKind     string    `json:"resume_kind,omitempty"`
	LeaseExpires   time.Time `json:"lease_expires_at,omitzero"`
	// ResumeAfter is the quota reset (round 7): a paused run with a
	// resume_after is reclaimable by its owner via any claim once it
	// passes. Deliberately separate from LeaseExpires — one column, one
	// meaning; the sweep reads were dual-purpose before.
	ResumeAfter   time.Time `json:"resume_after,omitzero"`
	Attempt       int       `json:"attempt"`
	Branch        string    `json:"branch,omitempty"`
	BaseSHA       string    `json:"base_sha,omitempty"`
	HeadSHA       string    `json:"head_sha,omitempty"`
	ResultSummary string    `json:"result_summary,omitempty"`
	Diffstat      string    `json:"diffstat,omitempty"`
	Checks        []Check   `json:"checks,omitempty"`
	LogRef        string    `json:"log_ref,omitempty"`
	FailReason    string    `json:"fail_reason,omitempty"`
	// Executor is how the worker ran the agent stage: "local", or the stage
	// profile's "wrapper:<hash>" / "sandbox:<hash>" (capability profiles §2).
	Executor  string    `json:"executor,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Check is one verification result reported by the worker.
type Check struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	Output string `json:"output,omitempty"`
}

// ClaimInput is what a worker brings to the queue: who it is and what it can
// run. ClaimNext matches it against the oldest queued run it may serve.
type ClaimInput struct {
	WorkerID   string   `json:"worker_id"`
	Runners    []string `json:"runners"`
	Workspaces []string `json:"workspaces"`
	KeyID      string   `json:"-"` // the claiming key (M5 binding)
}

// ReportInput is the worker's result for a claimed run.
type ReportInput struct {
	WorkerID string `json:"worker_id"`
	// Status: succeeded | failed (running heartbeats use Heartbeat, not Report)
	Status   Status  `json:"status"`
	Summary  string  `json:"summary"`
	HeadSHA  string  `json:"head_sha"`
	Diffstat string  `json:"diffstat"`
	Checks   []Check `json:"checks"`
	LogRef   string  `json:"log_ref"`
	Reason   string  `json:"reason"` // on failure
	// Branch/BaseSHA may be set by the worker if it resolved them (the plan
	// stage records the branch it will use; implement records the pushed head).
	Branch  string `json:"branch"`
	BaseSHA string `json:"base_sha"`
	// Executor: the stage profile that ran the agent (see Run.Executor).
	Executor string `json:"executor"`
}

// WorkspacePrefs is the per-workspace routing config: which runner each stage
// prefers. Empty = any runner matches. This is the whole routing engine:
// one axis (stage -> runner), per workspace, overridable per item at approval.
type WorkspacePrefs struct {
	Stages map[string]string `json:"stages"` // stage name -> runner name
}

// StageRunner resolves the preferred runner for a stage; "" = no preference.
func (p *WorkspacePrefs) StageRunner(stage string) string {
	if p == nil {
		return ""
	}
	return p.Stages[stage]
}

// QueueInput creates a run.
type QueueInput struct {
	WorkspaceID string
	ItemID      string
	PlanRevID   string
	Stage       string
	Branch      string
	BaseSHA     string
}
