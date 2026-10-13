package runs

import (
	"context"
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
	if err != nil && strings.Contains(err.Error(), "already has an active") {
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
			return nil, fmt.Errorf("%w: item already has an active %s run (%s)", ErrInvalidInput, in.Stage, r.ID)
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

// ClaimNext hands the worker its next run, or nil when it has nothing to do.
// Affinity first (its own paused/interrupted runs), then the queue filtered
// by scope and stage preference, then branch-resumable takeovers.
func (s *Service) ClaimNext(ctx context.Context, in ClaimInput) (*Run, error) {
	in.WorkerID = strings.TrimSpace(in.WorkerID)
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
	return s.storage.Claim(ctx, in, s.prefs, s.lease, s.now().UTC())
}

// Heartbeat extends the worker's lease; the first flips claimed -> running.
func (s *Service) Heartbeat(ctx context.Context, id, workerID string) error {
	id = strings.TrimSpace(id)
	if id == "" || strings.TrimSpace(workerID) == "" {
		return fmt.Errorf("%w: id and worker_id are required", ErrInvalidInput)
	}
	return s.storage.Heartbeat(ctx, id, workerID, s.lease, s.now().UTC())
}

// Report ends a run and chains: success queues the next configured stage or
// moves the item forward; failure blocks the item with the reason.
func (s *Service) Report(ctx context.Context, id string, in ReportInput) (*Run, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, fmt.Errorf("%w: id is required", ErrInvalidInput)
	}
	switch in.Status {
	case StatusSucceeded, StatusFailed:
	default:
		return nil, fmt.Errorf("%w: report status must be succeeded or failed", ErrInvalidInput)
	}
	if err := auth.RequireWorkspaceQuiet(ctx, ""); err == nil {
		// no-op: quiet check just validates a principal exists
	}
	run, err := s.storage.Report(ctx, id, in.WorkerID, in, s.now().UTC())
	if err != nil {
		return nil, err
	}
	// Scope: the reporting worker must hold the run (checked by storage) and
	// the run's workspace must be in the caller's scope.
	if err := auth.RequireWorkspaceQuiet(ctx, run.WorkspaceID); err != nil {
		return nil, fmt.Errorf("%w: run %s", ErrNotFound, id)
	}

	s.chain(ctx, run, in)
	return run, nil
}

// chain applies the item-side consequence of a finished run.
func (s *Service) chain(ctx context.Context, run *Run, in ReportInput) {
	if s.itemTransition == nil {
		return
	}
	switch run.Status {
	case StatusFailed:
		_ = s.itemTransition(ctx, run.ItemID, "blocked", "run failed: "+firstLine(in.Reason, in.Summary), "worker:"+run.WorkerID)
	case StatusSucceeded:
		switch run.Stage {
		case StagePlan:
			// The worker wrote the revision and enrichment. The item may
			// still be queued (the plan run IS its planning phase), so walk
			// the matrix: queued -> planning -> awaiting_approval.
			_ = s.itemTransition(ctx, run.ItemID, "planning", "", "worker:"+run.WorkerID)
			_ = s.itemTransition(ctx, run.ItemID, "awaiting_approval", "", "worker:"+run.WorkerID)
		case StageImplement, StageReview, StageQA:
			next := s.nextStage(run.WorkspaceID, run.Stage)
			// Review/QA run only when the workspace prefers a runner for
			// them; otherwise the sequence ends and Martin reviews.
			if next != "" && s.prefs[run.WorkspaceID].StageRunner(next) != "" {
				if _, err := s.Queue(ctx, QueueInput{
					WorkspaceID: run.WorkspaceID, ItemID: run.ItemID, PlanRevID: run.PlanRevID,
					Stage: next, Branch: run.Branch, BaseSHA: run.BaseSHA,
				}); err == nil {
					return
				}
			}
			_ = s.itemTransition(ctx, run.ItemID, "implementing", "", "worker:"+run.WorkerID)
			_ = s.itemTransition(ctx, run.ItemID, "in_review", "", "worker:"+run.WorkerID)
		}
	}
}

// Pause keeps the run with its worker (amendment, quota) — affinity means
// the resume lands where the session lives.
func (s *Service) Pause(ctx context.Context, id, workerID, reason string) error {
	id = strings.TrimSpace(id)
	if id == "" || strings.TrimSpace(workerID) == "" {
		return fmt.Errorf("%w: id and worker_id are required", ErrInvalidInput)
	}
	if err := s.checkScope(ctx, id); err != nil {
		return err
	}
	return s.storage.Pause(ctx, id, workerID, reason, s.now().UTC())
}

// Resume returns a paused run to its owner.
func (s *Service) Resume(ctx context.Context, id, workerID string) error {
	id = strings.TrimSpace(id)
	if id == "" || strings.TrimSpace(workerID) == "" {
		return fmt.Errorf("%w: id and worker_id are required", ErrInvalidInput)
	}
	if err := s.checkScope(ctx, id); err != nil {
		return err
	}
	return s.storage.Resume(ctx, id, workerID, s.lease, s.now().UTC())
}

// CancelItem retires an item's active runs (item rejected or done).
func (s *Service) CancelItem(ctx context.Context, itemID, reason string) error {
	itemID = strings.TrimSpace(itemID)
	if itemID == "" {
		return fmt.Errorf("%w: item is required", ErrInvalidInput)
	}
	_, err := s.storage.CancelItem(ctx, itemID, reason, s.now().UTC())
	return err
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
