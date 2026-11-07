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

// Phases a running implement run reports with its heartbeats, so the
// dashboard can show the Checks step running: the worker is running the
// repo's checks, or the implementer is fixing the ones that failed. A claim
// and a report clear it.
const (
	PhaseChecks = "checks"
	PhaseFixing = "fixing"
	// PhaseReviewing: a review-only item's branch_review run, past its
	// checks, is reviewing.
	PhaseReviewing = "reviewing"
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
	// StageSecondOpinion is an advisory review Martin asks for on an item
	// at in_review (or blocked by its review): one run, pinned to the
	// runner he picked and to the head it reviews. Its result is appended
	// to the item; it never moves or blocks it (tmp/second-opinion-review
	// — design.md).
	StageSecondOpinion = "second_opinion"
	// StageBranchReview reviews a review-only item's existing branch: the
	// repository's checks at its head, then a review (tmp/branch-handover
	// — design.md §4). One run on the workspace's review runner; whatever
	// the verdict, the item goes to Martin at in_review.
	StageBranchReview = "branch_review"
)

// ResumeKind: how a takeover may continue (affinity's escape hatch).
const (
	ResumeSession = "session" // same worker only — the agent session lives there
	ResumeBranch  = "branch"  // any worker, fresh context from the pushed branch
)

// takeoverGrace: an interrupted run becomes branch-resumable after this long
// without its owner reclaiming it (review M1).
const takeoverGrace = 30 * time.Minute

// Retryable failures (tmp/self-healing-workers — design.md §1): up to
// maxRetries requeues after the first failure, each after its back-off;
// from the second retry on the failing worker is kept off the run for
// avoidWindow past the back-off, so another worker gets the first go.
const (
	maxRetries  = 2
	avoidWindow = 30 * time.Minute
)

var retryBackoff = [maxRetries]time.Duration{time.Minute, 5 * time.Minute}

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
	Findings []Finding `json:"findings,omitempty"`
	// Pushed: whether an implement run pushed its branch to the remote; nil
	// when the worker did not say (older workers, other stages).
	Pushed *bool `json:"pushed,omitempty"`
	// PRURL: the pull request the worker opened (or reused) for the
	// implement branch, when the repo asks for one.
	PRURL string `json:"pr_url,omitempty"`
	// Setup: the setup commands an implement run ran in its fresh worktree
	// (none when the worktree was reused from an earlier round).
	Setup []Check `json:"setup,omitempty"`
	// FixRounds: implement only — the check fix rounds the run used.
	FixRounds int `json:"fix_rounds,omitempty"`
	// Phase: what a running implement run is doing past its first turn
	// (PhaseChecks / PhaseFixing), from its heartbeats; '' otherwise.
	Phase string `json:"phase,omitempty"`
	// Retries: how often a retryable failure sent the run back to the
	// queue; RetryAfter: not claimable before; AvoidWorker: the worker that
	// failed it last, kept off it until RetryAfter + avoidWindow.
	Retries     int       `json:"retries,omitempty"`
	RetryAfter  time.Time `json:"retry_after,omitzero"`
	AvoidWorker string    `json:"avoid_worker,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
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
	// Stopped: it never finished (check_timeout / quiet_timeout).
	Stopped bool `json:"stopped,omitempty"`
}

// ClaimInput is what a worker brings to the queue: who it is and what it can
// run. ClaimNext matches it against the oldest queued run it may serve.
type ClaimInput struct {
	WorkerID   string   `json:"worker_id"`
	Runners    []string `json:"runners"`
	Workspaces []string `json:"workspaces"`
	KeyID      string   `json:"-"` // the claiming key (M5 binding)
	// Presence: the worker's release and idle poll interval (decides when a
	// silent worker counts as offline).
	Version string `json:"version,omitempty"`
	PollMS  int64  `json:"poll_ms,omitempty"`
	// Unhealthy: offered workspaces whose preflight fails on the worker
	// (workspace → reason). The claim hands it none of their runs — not
	// even its own interrupted ones, which age into takeovers for other
	// workers (tmp/self-healing-workers — design.md §2).
	Unhealthy map[string]string `json:"unhealthy,omitempty"`
	// Indexable: offered workspaces whose repo the worker can fetch and
	// index (tmp/agent-mcp-and-indexing — design.md §3).
	Indexable []string `json:"indexable,omitempty"`
}

// IndexDuty is what a worker should index while idle: Enabled are its
// workspaces with the code_index switch on (it indexes the item branches
// it implemented there); Default are those whose default branch it keeps
// fresh — one able worker per workspace.
type IndexDuty struct {
	Enabled []string
	Default []string
}

// WorkerHealth is one change in a worker's health for one workspace, for
// the health notifier.
type WorkerHealth struct {
	Worker    string
	Workspace string
	Healthy   bool
	Reason    string // why it is unhealthy ("" when healthy again)
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
	// Pushed: implement only — false when the repo has no push configured,
	// so the head exists on the worker alone.
	Pushed *bool `json:"pushed,omitempty"`
	// PRURL: implement only — the branch's pull request (http(s) URL).
	PRURL string `json:"pr_url,omitempty"`
	// Setup: implement only — the setup commands run in a fresh worktree.
	Setup []Check `json:"setup,omitempty"`
	// FixRounds: implement only — the check fix rounds used.
	FixRounds int `json:"fix_rounds,omitempty"`
	// Verdict: a second opinion's pass/fail. It reports succeeded either
	// way (the review itself ran); the verdict is advisory.
	Verdict string `json:"verdict,omitempty"`
	// Retryable: a failure that was the worker's problem (setup, a hung
	// check, git, the push, the provider) — the run goes back to the queue
	// until its retries are spent, instead of blocking the item.
	Retryable bool `json:"retryable,omitempty"`
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
	// AgentsDefault is what an item without its own switch gets for a group
	// stage: AgentsPanel (the default when empty) or AgentsSolo.
	AgentsDefault string `json:"agents_default,omitempty"`
	// CodeIndex: idle workers keep the workspace's code index fresh.
	CodeIndex bool `json:"code_index,omitempty"`
}

// CodeIndexOn reports whether idle workers index the workspace (nil prefs =
// off).
func (p *WorkspacePrefs) CodeIndexOn() bool { return p != nil && p.CodeIndex }

// Per-item multi-agent switch values (inbox item "agents").
const (
	AgentsPanel = "panel"
	AgentsSolo  = "solo"
)

// Panel decides whether a group stage runs as a panel for an item whose
// switch is itemAgents ("" = the workspace default).
func (p *WorkspacePrefs) Panel(itemAgents string) bool {
	if itemAgents != "" {
		return itemAgents != AgentsSolo
	}
	return p == nil || p.AgentsDefault != AgentsSolo
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
	// HeadSHA pins a second opinion to the commit it reviews.
	HeadSHA string
}
