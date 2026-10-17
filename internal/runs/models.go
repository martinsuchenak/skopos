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
	// Advisory plan stages (multi-agent planning, design §4): after a plan
	// group's candidates, an optional judge (compare) and an optional merge
	// (synthesize, which writes one more candidate). Single runner each;
	// a failure never blocks — the gate opens without them.
	StageCompare    = "compare"
	StageSynthesize = "synthesize"
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
	Executor string `json:"executor,omitempty"`
	// GroupID is set when the run is one member of a stage group (several
	// agents on one stage); Focus names the member's brief
	// (.skopos/focus/<name>.md, read at the approved base). Both empty for
	// an ordinary run.
	GroupID string `json:"group_id,omitempty"`
	Focus   string `json:"focus,omitempty"`
	// CostUSD is what the agent run cost, when the runner reports it.
	CostUSD *float64 `json:"cost_usd,omitempty"`
	// Findings are a review/QA member's structured findings.
	Findings  []Finding `json:"findings,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Finding is one review/QA observation. Blocking findings fail the member;
// nits are reported but never block.
type Finding struct {
	Severity string `json:"severity"` // blocking | nit
	Text     string `json:"text"`
	File     string `json:"file,omitempty"`
}

const (
	SeverityBlocking = "blocking"
	SeverityNit      = "nit"
)

// Group is N runs of one stage for one item, decided once by Policy
// (tmp/multi-agent-stages — design.md §1).
type Group struct {
	ID          string    `json:"id"`
	WorkspaceID string    `json:"workspace_id"`
	ItemID      string    `json:"item_id"`
	Stage       string    `json:"stage"`
	Round       int       `json:"round"`
	Size        int       `json:"size"`
	Policy      string    `json:"policy"`
	EarlyExit   bool      `json:"early_exit"`
	State       string    `json:"state"`             // open | decided | cancelled
	Outcome     string    `json:"outcome,omitempty"` // pass | fail | candidates | timeout
	BaseSHA     string    `json:"base_sha,omitempty"`
	Deadline    time.Time `json:"deadline_at"`
	DecidedAt   time.Time `json:"decided_at,omitzero"`
	CreatedAt   time.Time `json:"created_at"`
}

// Group states and outcomes.
const (
	GroupOpen      = "open"
	GroupDecided   = "decided"
	GroupCancelled = "cancelled"

	OutcomePass       = "pass"
	OutcomeFail       = "fail"
	OutcomeCandidates = "candidates"
	OutcomeTimeout    = "timeout"
)

// Review/QA group policies (plan groups use PolicyCandidates).
const (
	PolicyAnyBlocks  = "any-blocks" // any blocking finding fails the group (default)
	PolicyMajority   = "majority"   // more than half of the members fail
	PolicyAllPass    = "all-pass"   // like any-blocks, but never exits early
	PolicyCandidates = "candidates" // plan: every member's result is a candidate
)

// DefaultGroupTimeout bounds how long a group waits for its members.
const DefaultGroupTimeout = 60 * time.Minute

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
	// CostUSD: the agent run's cost when the runner reports one.
	CostUSD *float64 `json:"cost_usd,omitempty"`
	// Findings: review/QA structured findings (severity + text).
	Findings []Finding `json:"findings,omitempty"`
}

// WorkspacePrefs is the per-workspace routing config: which runner each stage
// prefers. Empty = any runner matches. This is the whole routing engine:
// one axis (stage -> runner), per workspace, overridable per item at approval.
type WorkspacePrefs struct {
	Stages map[string]string `json:"stages"` // stage name -> runner name (one member)
	// Groups: stages routed to two or more members (multi-agent stages).
	// Opt-in — a stage listed here never also appears in Stages.
	Groups map[string][]Member `json:"groups,omitempty"`
	// Policies: how a group stage is decided (defaults: any-blocks, no
	// early exit, DefaultGroupTimeout).
	Policies map[string]StagePolicy `json:"policies,omitempty"`
}

// Member is one agent of a group stage: a runner, optionally with a focus
// brief.
type Member struct {
	Runner string `json:"runner"`
	Focus  string `json:"focus,omitempty"`
}

// StagePolicy decides a group stage.
type StagePolicy struct {
	Policy    string        `json:"policy"`
	EarlyExit bool          `json:"early_exit"`
	Timeout   time.Duration `json:"timeout"`
}

// StageRunner resolves the preferred runner for a single-member stage;
// "" = no preference (or the stage is a group).
func (p *WorkspacePrefs) StageRunner(stage string) string {
	if p == nil {
		return ""
	}
	return p.Stages[stage]
}

// GroupMembers returns the members of a group stage; nil when the stage
// runs on one agent.
func (p *WorkspacePrefs) GroupMembers(stage string) []Member {
	if p == nil || len(p.Groups[stage]) < 2 {
		return nil
	}
	return p.Groups[stage]
}

// SingleFocus is the focus brief of a stage routed to exactly one focused
// member (review=security@claude); "" otherwise.
func (p *WorkspacePrefs) SingleFocus(stage string) string {
	if p == nil || len(p.Groups[stage]) != 1 {
		return ""
	}
	return p.Groups[stage][0].Focus
}

// Configured: the workspace routes this stage at all (a runner or a group).
// Review/QA run only when configured.
func (p *WorkspacePrefs) Configured(stage string) bool {
	return p.StageRunner(stage) != "" || p.GroupMembers(stage) != nil
}

// PolicyFor returns the stage's group policy with defaults filled in.
func (p *WorkspacePrefs) PolicyFor(stage string) StagePolicy {
	var sp StagePolicy
	if p != nil {
		sp = p.Policies[stage]
	}
	if sp.Policy == "" {
		sp.Policy = PolicyAnyBlocks
		if stage == StagePlan {
			sp.Policy = PolicyCandidates
		}
	}
	if sp.Timeout <= 0 {
		sp.Timeout = DefaultGroupTimeout
	}
	return sp
}

// QueueInput creates a run.
type QueueInput struct {
	WorkspaceID string
	ItemID      string
	PlanRevID   string
	Stage       string
	Branch      string
	BaseSHA     string
	// Runner pins the run to a runner regardless of the workspace
	// preference; Single never opens a group (a seeded plan revise).
	Runner string
	Single bool
}
