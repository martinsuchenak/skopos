package runs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/martinsuchenak/skopos/internal/auth"
	"github.com/martinsuchenak/skopos/internal/ids"
)

// Service is the run queue. Workers pull: ClaimNext asks for work, Report
// ends a stage. Chaining on success (implement -> review -> qa -> in_review)
// is decided here per the workspace's stage sequence; the item-side status
// moves go through the ItemTransition callback wired in serve.go so the
// workflow matrix stays the single authority on item states.
type Service struct {
	storage *Storage
	now     func() time.Time
	lease   time.Duration

	prefs map[string]*WorkspacePrefs

	// itemTransition moves the inbox item (system edge) — wired to
	// inbox.SystemTransition. Nil disables chaining (tests, partial wiring).
	itemTransition func(ctx context.Context, itemID, to, reason, agentID string) error
	// itemAppend writes a section into the item (a stage group's summary) —
	// wired to inbox.AppendSection. Nil skips the summary.
	itemAppend func(ctx context.Context, itemID, heading, body string) error
	// nextStage resolves the stage that follows a successful one per
	// workspace; "" ends the sequence. Defaults to: plan -> implement -> done
	// (review/qa only when configured).
	nextStage func(workspaceID, stage string) string
}

func NewService(storage *Storage) *Service {
	s := &Service{storage: storage, now: time.Now, lease: 10 * time.Minute, prefs: map[string]*WorkspacePrefs{}}
	s.nextStage = defaultStageSequence
	return s
}

// SetLease overrides the claim lease (tests).
func (s *Service) SetLease(d time.Duration) { s.lease = d }

// SetPrefs installs the per-workspace routing preferences.
func (s *Service) SetPrefs(p map[string]*WorkspacePrefs) {
	if p == nil {
		p = map[string]*WorkspacePrefs{}
	}
	s.prefs = p
}

// SetItemAppend wires the inbox section-append callback (group summaries).
func (s *Service) SetItemAppend(fn func(ctx context.Context, itemID, heading, body string) error) {
	s.itemAppend = fn
}

// SetItemTransition wires the inbox system-transition callback.
func (s *Service) SetItemTransition(fn func(ctx context.Context, itemID, to, reason, agentID string) error) {
	s.itemTransition = fn
}

// defaultStageSequence: plan then implement; review and qa run only when the
// workspace configures a preference for them (the second-LLM review stage).
func defaultStageSequence(_, stage string) string {
	switch stage {
	case StagePlan:
		return StageImplement
	case StageImplement:
		return StageReview // only queued when the workspace prefers a reviewer
	case StageReview:
		return StageQA // same: queued only when configured
	}
	return ""
}

// QueueItem is the inbox hook shape: queue a stage run for an item. It
// swallows duplicate-active errors (idempotent with the workflow's own
// guards) but surfaces real failures.
func (s *Service) QueueItem(ctx context.Context, itemID, workspaceID, stage string) error {
	_, err := s.Queue(ctx, QueueInput{WorkspaceID: workspaceID, ItemID: itemID, Stage: stage})
	if err != nil && errors.Is(err, ErrDuplicateActive) {
		return nil
	}
	return err
}

// Queue adds a run for a stage of an item. Duplicate active runs for the
// same item+stage are refused (idempotent chaining).
func (s *Service) Queue(ctx context.Context, in QueueInput) (*Run, error) {
	in.WorkspaceID = strings.TrimSpace(in.WorkspaceID)
	in.ItemID = strings.TrimSpace(in.ItemID)
	in.Stage = strings.TrimSpace(in.Stage)
	if in.WorkspaceID == "" || in.ItemID == "" || in.Stage == "" {
		return nil, fmt.Errorf("%w: workspace, item and stage are required", ErrInvalidInput)
	}
	switch in.Stage {
	case StagePlan, StageImplement, StageReview, StageQA:
	default:
		return nil, fmt.Errorf("%w: unknown stage %q", ErrInvalidInput, in.Stage)
	}
	if err := auth.RequireWorkspace(ctx, in.WorkspaceID); err != nil {
		return nil, err
	}
	existing, err := s.storage.List(ctx, "", in.ItemID)
	if err != nil {
		return nil, err
	}
	for _, r := range existing {
		if r.Stage == in.Stage && (r.Status == StatusQueued || r.Status == StatusClaimed || r.Status == StatusRunning || r.Status == StatusPaused) {
			return nil, fmt.Errorf("%w: %s (%s)", ErrDuplicateActive, in.Stage, r.ID)
		}
	}
	// Multi-agent stage (opt-in): two or more members routed to it open a
	// group instead of a single run.
	if groupStage(in.Stage) {
		if members := s.prefs[in.WorkspaceID].GroupMembers(in.Stage); members != nil {
			return s.queueGroup(ctx, in, members)
		}
	}
	now := s.now().UTC()
	r := Run{
		ID:          ids.New(),
		WorkspaceID: in.WorkspaceID,
		ItemID:      in.ItemID,
		PlanRevID:   strings.TrimSpace(in.PlanRevID),
		Stage:       in.Stage,
		Runner:      s.prefs[in.WorkspaceID].StageRunner(in.Stage),
		Focus:       s.prefs[in.WorkspaceID].SingleFocus(in.Stage),
		Branch:      strings.TrimSpace(in.Branch),
		BaseSHA:     strings.TrimSpace(in.BaseSHA),
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := s.storage.Create(ctx, r); err != nil {
		return nil, err
	}
	return &r, nil
}

// phaseForStage is the item status a run's stage corresponds to: a plan
// run IS the item's planning phase, an implement/review/qa run its
// implementing phase. The item moves in on claim so a mid-run failure
// blocks from a valid edge (review H1).
func phaseForStage(stage string) string {
	switch stage {
	case StagePlan:
		return "planning"
	case StageImplement, StageReview, StageQA:
		return "implementing"
	}
	return ""
}

// ClaimNext hands the worker its next run, or nil when it has nothing to do.
// Affinity first (its own interrupted runs), then the queue filtered by
// scope and stage preference, then branch-resumable takeovers.
func (s *Service) ClaimNext(ctx context.Context, in ClaimInput) (*Run, error) {
	in.WorkerID = strings.TrimSpace(in.WorkerID)
	// F2 (round 6): record the claiming key — the fenced mutations later
	// match on it, so a second key sharing the workspace cannot touch the
	// run even with the right worker_id and token.
	in.KeyID = callerKeyID(ctx)
	if in.WorkerID == "" {
		return nil, fmt.Errorf("%w: worker_id is required", ErrInvalidInput)
	}
	if len(in.Workspaces) == 0 {
		return nil, fmt.Errorf("%w: workspaces is required (scope at least one)", ErrInvalidInput)
	}
	for _, ws := range in.Workspaces {
		if err := auth.RequireWorkspaceQuiet(ctx, ws); err != nil {
			return nil, fmt.Errorf("%w: workspace scope", ErrNotFound)
		}
	}
	run, err := s.storage.Claim(ctx, in, s.prefs, s.lease, s.now().UTC())
	if err != nil || run == nil {
		return run, err
	}
	// The item enters the run's phase on claim (H1). Re-claims of an item
	// already in phase fail the matrix quietly — that is fine.
	if to := phaseForStage(run.Stage); to != "" && s.itemTransition != nil {
		_ = s.itemTransition(ctx, run.ItemID, to, "run claimed: "+run.ID, "worker:"+run.WorkerID)
	}
	return run, nil
}

// Heartbeat extends the worker's lease; the first flips claimed -> running.
func (s *Service) Heartbeat(ctx context.Context, id, workerID, leaseToken string) error {
	id = strings.TrimSpace(id)
	if id == "" || strings.TrimSpace(workerID) == "" {
		return fmt.Errorf("%w: id and worker_id are required", ErrInvalidInput)
	}
	if err := s.checkScope(ctx, id); err != nil {
		return err
	}
	keyID := callerKeyID(ctx)
	return s.storage.Heartbeat(ctx, id, workerID, keyID, hashLeaseToken(leaseToken), s.lease, s.now().UTC())
}

// Report ends a run and chains: success queues the next configured stage or
// moves the item forward; failure blocks the item with the reason.
func (s *Service) Report(ctx context.Context, id string, in ReportInput, leaseToken string) (*Run, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, fmt.Errorf("%w: id is required", ErrInvalidInput)
	}
	switch in.Status {
	case StatusSucceeded, StatusFailed:
	default:
		return nil, fmt.Errorf("%w: report status must be succeeded or failed", ErrInvalidInput)
	}
	// Scope BEFORE the write: the storage layer trusts the request's
	// worker_id for ownership, so the caller's workspace scope is the only
	// real gate on who may mutate a run (review finding 1).
	if err := s.checkScope(ctx, id); err != nil {
		return nil, err
	}
	// The executor id is a short worker-chosen label ("local",
	// "wrapper:<12 hex>"); bound it like any client string.
	if in.Executor = strings.TrimSpace(in.Executor); len(in.Executor) > 64 {
		in.Executor = in.Executor[:64]
	}
	run, err := s.storage.Report(ctx, id, in.WorkerID, callerKeyID(ctx), hashLeaseToken(leaseToken), in, s.now().UTC())
	if err != nil {
		return nil, err
	}
	s.chain(ctx, run, in)
	return run, nil
}

// chain applies the item-side consequence of a finished run. Item-move
// failures are recorded on the run (the worker's report stays truthful
// about the stage; the chain outcome lands in fail_reason for diagnosis).
func (s *Service) chain(ctx context.Context, run *Run, in ReportInput) {
	if s.itemTransition == nil {
		return
	}
	// A group member's result is recorded on its run; the group decides
	// (once) and moves the item — never the member alone.
	if run.GroupID != "" {
		s.groupMemberDone(ctx, run)
		return
	}
	move := func(to, reason string) {
		if err := s.itemTransition(ctx, run.ItemID, to, reason, "worker:"+run.WorkerID); err != nil {
			s.chainNote(ctx, run, "item move to "+to+" failed: "+err.Error())
		}
	}
	switch run.Status {
	case StatusFailed:
		move("blocked", "run failed: "+firstLine(in.Reason, in.Summary))
	case StatusSucceeded:
		switch run.Stage {
		case StagePlan:
			// The worker wrote the revision and enrichment. The item may
			// still be queued (the plan run IS its planning phase), so walk
			// the matrix: queued -> planning -> awaiting_approval.
			move("planning", "")
			move("awaiting_approval", "")
		case StageImplement, StageReview, StageQA:
			s.advance(ctx, run, "worker:"+run.WorkerID)
		}
	}
}

// advance continues after a successful implement/review/qa stage (a single
// run, or a group's passing decision): queue the next configured stage, or
// hand the item to Martin at in_review.
func (s *Service) advance(ctx context.Context, run *Run, agentID string) {
	next := s.nextStage(run.WorkspaceID, run.Stage)
	// Review/QA run only when the workspace configures them (one runner or
	// a group); otherwise the sequence ends and Martin reviews.
	if next != "" && s.prefs[run.WorkspaceID].Configured(next) {
		if _, err := s.Queue(ctx, QueueInput{
			WorkspaceID: run.WorkspaceID, ItemID: run.ItemID, PlanRevID: run.PlanRevID,
			Stage: next, Branch: run.Branch, BaseSHA: run.BaseSHA,
		}); err == nil {
			return
		}
	}
	_ = s.itemTransition(ctx, run.ItemID, "implementing", "", agentID)
	_ = s.itemTransition(ctx, run.ItemID, "in_review", "", agentID)
}

// Pause keeps the run with its worker — affinity means the resume lands
// where the session lives. A non-zero resumeAfter (quota: the allowance
// reset) makes the pause restart-safe: the owner reclaims it via any claim
// once the time passes, even after a worker restart (A2).
func (s *Service) Pause(ctx context.Context, id, workerID, reason string, resumeAfter time.Time, leaseToken string) error {
	id = strings.TrimSpace(id)
	if id == "" || strings.TrimSpace(workerID) == "" {
		return fmt.Errorf("%w: id and worker_id are required", ErrInvalidInput)
	}
	if err := s.checkScope(ctx, id); err != nil {
		return err
	}
	return s.storage.Pause(ctx, id, workerID, reason, resumeAfter, callerKeyID(ctx), hashLeaseToken(leaseToken), s.now().UTC())
}

// CancelOwn lets the owning worker retire its own run (abandoned stages —
// A4: an unexecuted run are a cancellation, not a failure).
func (s *Service) CancelOwn(ctx context.Context, id, workerID, reason, leaseToken string) error {
	id = strings.TrimSpace(id)
	if id == "" || strings.TrimSpace(workerID) == "" {
		return fmt.Errorf("%w: id and worker_id are required", ErrInvalidInput)
	}
	r, err := s.storage.Get(ctx, id)
	if err != nil {
		return err
	}
	if err := auth.RequireWorkspaceQuiet(ctx, r.WorkspaceID); err != nil {
		return fmt.Errorf("%w: run %s", ErrNotFound, id)
	}
	if r.WorkerID != workerID {
		return ErrLeaseHeld
	}
	return s.storage.CancelOne(ctx, id, workerID, reason, callerKeyID(ctx), hashLeaseToken(leaseToken), s.now().UTC())
}

// CancelItem retires an item's active runs (item rejected or done).
func (s *Service) CancelItem(ctx context.Context, itemID, reason string) error {
	itemID = strings.TrimSpace(itemID)
	if itemID == "" {
		return fmt.Errorf("%w: item is required", ErrInvalidInput)
	}
	// Open groups close first (cancelled, no decision), so no member's late
	// report can decide them; then every active run is retired.
	if groups, err := s.storage.OpenGroupsForItem(ctx, itemID); err == nil {
		for _, g := range groups {
			_, _ = s.storage.CancelGroup(ctx, g.ID, reason, s.now().UTC())
		}
	}
	_, err := s.storage.CancelItem(ctx, itemID, reason, s.now().UTC())
	return err
}

// ResumeItem resumes an item's paused run back to its owner (system action:
// approving an amendment re-arms the run it paused; review H4).
func (s *Service) ResumeItem(ctx context.Context, itemID string) error {
	runs, err := s.storage.List(ctx, StatusPaused, strings.TrimSpace(itemID))
	if err != nil {
		return err
	}
	for i := range runs {
		_ = s.storage.Resume(ctx, runs[i].ID, runs[i].WorkerID, s.lease, s.now().UTC())
	}
	return nil
}

// ListWorkers is the worker registry (capabilities recorded at claim).
func (s *Service) ListWorkers(ctx context.Context) ([]WorkerSeen, error) {
	return s.storage.ListWorkers(ctx)
}

// RunSummary is the inbox-facing slice of a run — enough for the Slack
// relay's gate messages and the `done` command's head-sha lookup.
type RunSummary struct {
	ID       string `json:"id"`
	Stage    string `json:"stage"`
	Status   string `json:"status"`
	Branch   string `json:"branch,omitempty"`
	HeadSHA  string `json:"head_sha,omitempty"`
	Diffstat string `json:"diffstat,omitempty"`
	Summary  string `json:"result_summary,omitempty"`
	// GroupID: the run was one member of a multi-agent stage (its group's
	// summary section speaks for the decision, not this member).
	GroupID string `json:"group_id,omitempty"`
}

// ListRuns is the inbox-facing read (amendment-paused check).
func (s *Service) ListRuns(ctx context.Context, itemID string) ([]RunSummary, error) {
	all, err := s.storage.List(ctx, "", strings.TrimSpace(itemID))
	if err != nil {
		return nil, err
	}
	out := make([]RunSummary, 0, len(all))
	for _, r := range all {
		out = append(out, RunSummary{ID: r.ID, Stage: r.Stage, Status: string(r.Status),
			Branch: r.Branch, HeadSHA: r.HeadSHA, Diffstat: r.Diffstat, Summary: r.ResultSummary, GroupID: r.GroupID})
	}
	return out, nil
}

// QueueStage is the inbox hook for a stage with the approved revision bound
// (review H3): the implement run carries plan_rev_id and base_sha. It
// returns the run id so a failed follow-up can cancel exactly that run
// (review N1: item-wide CancelItem as compensation killed unrelated runs).
func (s *Service) QueueStage(ctx context.Context, itemID, workspaceID, stage, planRevID, baseSHA, branch string) (string, error) {
	r, err := s.Queue(ctx, QueueInput{
		WorkspaceID: workspaceID, ItemID: itemID, Stage: stage,
		PlanRevID: planRevID, BaseSHA: baseSHA, Branch: branch,
	})
	if err != nil {
		if errors.Is(err, ErrDuplicateActive) {
			return "", nil
		}
		return "", err
	}
	return r.ID, nil
}

// CancelRun cancels one run by id (targeted compensation, review N1). It
// only ever targets a run THIS service just queued (unclaimed: empty
// worker/key/token), which the fenced match permits; claimed runs belong to
// their worker's CancelOwn.
func (s *Service) CancelRun(ctx context.Context, id, reason string) error {
	return s.storage.CancelOne(ctx, strings.TrimSpace(id), "", reason, "", "", s.now().UTC())
}

// Get and List are the read surface (dashboard, tests).
func (s *Service) Get(ctx context.Context, id string) (*Run, error) {
	r, err := s.storage.Get(ctx, strings.TrimSpace(id))
	if err != nil {
		return nil, err
	}
	if err := auth.RequireWorkspaceQuiet(ctx, r.WorkspaceID); err != nil {
		return nil, fmt.Errorf("%w: run %s", ErrNotFound, id)
	}
	return r, nil
}

func (s *Service) List(ctx context.Context, status Status, itemID string) ([]Run, error) {
	if auth.ScopedContext(ctx) {
		// Scoped keys see their slice: filter after read (runs are few).
		all, err := s.storage.List(ctx, status, itemID)
		if err != nil {
			return nil, err
		}
		p := auth.PrincipalFromContext(ctx)
		out := make([]Run, 0, len(all))
		for _, r := range all {
			if p.CanAccess(r.WorkspaceID) {
				out = append(out, r)
			}
		}
		return out, nil
	}
	return s.storage.List(ctx, status, itemID)
}

// callerKeyID is the requesting key ("" for root/internal callers), recorded
// at claim time and matched by every fenced mutation (F2).
func callerKeyID(ctx context.Context) string {
	if p := auth.PrincipalFromContext(ctx); p != nil {
		return p.KeyID
	}
	return ""
}

// checkWorkspace: the caller may read this workspace's runs and groups.
func (s *Service) checkWorkspace(ctx context.Context, workspaceID string) error {
	return auth.RequireWorkspaceQuiet(ctx, workspaceID)
}

func (s *Service) checkScope(ctx context.Context, id string) error {
	r, err := s.storage.Get(ctx, id)
	if err != nil {
		return err
	}
	if err := auth.RequireWorkspaceQuiet(ctx, r.WorkspaceID); err != nil {
		return fmt.Errorf("%w: run %s", ErrNotFound, id)
	}
	return nil
}

// chainNote records a chain failure on the run (best-effort; serves as the
// diagnosable trail when the item and run disagree).
func (s *Service) chainNote(ctx context.Context, run *Run, note string) {
	_ = s.storage.Note(ctx, run.ID, note)
}

func firstLine(xs ...string) string {
	for _, x := range xs {
		if x != "" {
			if i := strings.IndexByte(x, '\n'); i >= 0 {
				return x[:i]
			}
			return x
		}
	}
	return "no reason given"
}
