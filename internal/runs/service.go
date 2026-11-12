package runs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"sync"
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
	// logWake: readers long-polling a run's log (logs.go).
	logMu   sync.Mutex
	logWake map[string]chan struct{}

	// prefs backs SetPrefs (tests, partial wiring); prefsFor is the lookup
	// every queue and claim reads — the pipeline settings resolver in
	// production, so routing changes apply at the next queue.
	prefs    map[string]*WorkspacePrefs
	prefsFor func(ctx context.Context, workspaceID string) *WorkspacePrefs

	// itemTransition moves the inbox item (system edge) — wired to
	// inbox.SystemTransition. Nil disables chaining (tests, partial wiring).
	itemTransition func(ctx context.Context, itemID, to, reason, agentID string) error
	// itemAppend writes a section into the item (a stage group's summary) —
	// wired to inbox.AppendSection. Nil skips the summary.
	itemAppend func(ctx context.Context, itemID, heading, body string) error
	// audit records worker pauses and resumes (nil = none).
	audit AuditRecorder
	// questionAsker records a run's questions (internal/qa, via serve.go).
	questionAsker QuestionAsker
	// questionReader reads the record for a run's prompt.
	questionReader QuestionReader
	// secondOpinionNotify hears about every finished second opinion (the
	// Slack relay posts it to the item's thread).
	secondOpinionNotify func(ctx context.Context, r SecondOpinionResult)
	// healthNotify hears when a worker turns unhealthy for a workspace, or
	// recovers (the Slack relay posts it once).
	healthNotify func(ctx context.Context, h WorkerHealth)
	// Plan groups (multi-agent planning), wired in serve.go: planRoundStart
	// closes the previous round (supersedes its open candidates) when a new
	// plan group opens; ensurePlan creates and links the item's plan when
	// the first member is claimed (the item is in planning by then), so
	// planners only ever write candidates.
	planRoundStart func(ctx context.Context, itemID string) error
	ensurePlan     func(ctx context.Context, itemID string) error
	// itemAgents reads the item's multi-agent switch ("panel", "solo", ""
	// = workspace default) — wired to the inbox. Nil = always the default.
	itemAgents func(ctx context.Context, itemID string) string
	// nextStage resolves the stage that follows a successful one per
	// workspace; "" ends the sequence. Defaults to: plan -> implement -> done
	// (review/qa only when configured).
	nextStage func(workspaceID, stage string) string
	// configRev / configDigest: the workspace config source (SetConfigSource).
	configRev    func(ctx context.Context, workspaceID string) string
	configDigest func(ctx context.Context, workspaces []string) string
	// fleetRev: the runner definitions' revision (SetFleetRev).
	fleetRev func(ctx context.Context) string
	// runMCP / mcpManaged: the MCP servers a run's claim answer carries,
	// and whether the server holds a workspace's (SetMCPSource).
	runMCP     func(ctx context.Context, workspaceID string, serverCommands bool) (*RunMCP, error)
	mcpManaged func(ctx context.Context, workspaceID string) bool
}

// UnreadableConfigRev is the revision a claim names when the workspace's
// config could not be resolved or recorded: no fetch answers it, so the
// worker gives the run back instead of running it with other settings.
const UnreadableConfigRev = "unreadable"

// SetConfigSource wires the workspace config (internal/wsconfig): rev names
// what applies to a workspace now ("" = not managed), digest the configs of
// a set of workspaces together (the claim answer's Skopos-Config-Rev).
func (s *Service) SetConfigSource(rev func(ctx context.Context, workspaceID string) string, digest func(ctx context.Context, workspaces []string) string) {
	s.configRev, s.configDigest = rev, digest
}

// SetFleetRev wires the revision of the runner definitions every claim
// answer carries (Skopos-Fleet-Rev).
func (s *Service) SetFleetRev(fn func(ctx context.Context) string) { s.fleetRev = fn }

// SetMCPSource wires where a claimed run's MCP servers come from: run
// resolves them (with their credentials) for the run's workspace, managed
// says whether the server holds a workspace's MCP servers at all.
func (s *Service) SetMCPSource(run func(ctx context.Context, workspaceID string, serverCommands bool) (*RunMCP, error), managed func(ctx context.Context, workspaceID string) bool) {
	s.runMCP, s.mcpManaged = run, managed
}

// FleetRev is the runner definitions' revision ("" = none defined).
func (s *Service) FleetRev(ctx context.Context) string {
	if s.fleetRev == nil {
		return ""
	}
	return s.fleetRev(ctx)
}

// ConfigDigest names the effective configs of the workspaces a claim
// offers ("" = none of them is managed, or no config source is wired).
func (s *Service) ConfigDigest(ctx context.Context, workspaces []string) string {
	if s.configDigest == nil {
		return ""
	}
	return s.configDigest(ctx, workspaces)
}

func NewService(storage *Storage) *Service {
	s := &Service{storage: storage, now: time.Now, lease: 10 * time.Minute, prefs: map[string]*WorkspacePrefs{}}
	s.nextStage = defaultStageSequence
	return s
}

// SetLease overrides the claim lease (tests).
func (s *Service) SetLease(d time.Duration) { s.lease = d }

// SetPrefs installs fixed per-workspace routing (tests, partial wiring).
func (s *Service) SetPrefs(p map[string]*WorkspacePrefs) {
	if p == nil {
		p = map[string]*WorkspacePrefs{}
	}
	s.prefs = p
	s.prefsFor = nil
}

// SetPrefsSource installs the live routing lookup (the pipeline settings
// resolver): read at every queue and claim.
func (s *Service) SetPrefsSource(fn func(ctx context.Context, workspaceID string) *WorkspacePrefs) {
	s.prefsFor = fn
}

// prefsOf is the routing of one workspace (nil = nothing configured).
func (s *Service) prefsOf(ctx context.Context, workspaceID string) *WorkspacePrefs {
	if s.prefsFor != nil {
		return s.prefsFor(ctx, workspaceID)
	}
	return s.prefs[workspaceID]
}

// SetPlanHooks wires the plan-group callbacks (see the fields).
func (s *Service) SetPlanHooks(roundStart, ensurePlan func(ctx context.Context, itemID string) error) {
	s.planRoundStart, s.ensurePlan = roundStart, ensurePlan
}

// SetItemAgents wires the per-item multi-agent switch lookup.
func (s *Service) SetItemAgents(fn func(ctx context.Context, itemID string) string) {
	s.itemAgents = fn
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
	case StagePlan, StageImplement, StageReview, StageQA, StageCompare, StageSynthesize:
	case StageSecondOpinion:
		// Advisory and explicit: the runner the approver picked and the head it
		// reviews, never routed through pipeline settings.
		if strings.TrimSpace(in.Runner) == "" || strings.TrimSpace(in.HeadSHA) == "" {
			return nil, fmt.Errorf("%w: a second opinion needs a runner and the head it reviews", ErrInvalidInput)
		}
		in.Single = true
	case StageBranchReview:
		// Routed like the review stage, so a branch is reviewed the way an
		// implementation would be: the review panel (each member with its
		// focus) for an item on panel, else one reviewer — the review
		// runner, or the panel's first member without its focus (solo).
		in.Single = true
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
	// group instead of a single run (a seeded revise is always single).
	// The item's switch decides, at every queue: panel (or the workspace
	// default) opens the group; solo runs the first member alone, without
	// its focus brief.
	route := routeStage(in.Stage)
	if groupStage(in.Stage) && (!in.Single || in.Stage == StageBranchReview) {
		if members := s.prefsOf(ctx, in.WorkspaceID).GroupMembers(route); members != nil {
			if s.prefsOf(ctx, in.WorkspaceID).Panel(s.agentsFor(ctx, in.ItemID)) {
				return s.queueGroup(ctx, in, members)
			}
			if strings.TrimSpace(in.Runner) == "" {
				in.Runner = members[0].Runner
			}
		}
	}
	now := s.now().UTC()
	runner := runnerFor(in, s.prefsOf(ctx, in.WorkspaceID))
	preferWorker, preferUntil := "", time.Time{}
	if w := lastWorkerFor(existing, in.Stage, runner); w != "" {
		preferWorker, preferUntil = w, now.Add(preferGrace)
	}
	r := Run{
		ID:           ids.New(),
		WorkspaceID:  in.WorkspaceID,
		ItemID:       in.ItemID,
		PlanRevID:    strings.TrimSpace(in.PlanRevID),
		Stage:        in.Stage,
		Runner:       runner,
		Focus:        s.prefsOf(ctx, in.WorkspaceID).SingleFocus(route),
		Branch:       strings.TrimSpace(in.Branch),
		BaseSHA:      strings.TrimSpace(in.BaseSHA),
		HeadSHA:      strings.TrimSpace(in.HeadSHA),
		CreatedAt:    now,
		UpdatedAt:    now,
		PreferWorker: preferWorker,
		PreferUntil:  preferUntil,
	}
	if err := s.storage.Create(ctx, r); err != nil {
		return nil, err
	}
	return &r, nil
}

// lastWorkerFor is the worker that ran the item's newest run of this stage
// (existing is newest first): a run queued again for the item (Retry, change
// requests) prefers it. Only the stages that work in the clone gain from it
// — implement (the branch, the setup, uncommitted work) and branch_review
// (the setup) — and only when the newest run was a single run on the same
// runner: a panel member's worker, or another runner, may be elsewhere.
func lastWorkerFor(existing []Run, stage, runner string) string {
	if stage != StageImplement && stage != StageBranchReview {
		return ""
	}
	for _, r := range existing {
		if r.Stage == stage && r.WorkerID != "" {
			// The same runner: the name it was queued for, or — for a run
			// nothing pins — one the server picked last time (the claim
			// writes the pick to the run; the new run has none yet).
			same := r.Runner == runner || (runner == "" && r.RunnerPicked)
			if r.GroupID != "" || !same {
				return ""
			}
			return r.WorkerID
		}
	}
	return ""
}

// agentsFor reads the item's multi-agent switch; "" (workspace default)
// when unwired or unreadable.
func (s *Service) agentsFor(ctx context.Context, itemID string) string {
	if s.itemAgents == nil {
		return ""
	}
	return s.itemAgents(ctx, itemID)
}

// runnerFor: an explicit runner (a seeded revise) wins over the workspace
// preference.
func runnerFor(in QueueInput, p *WorkspacePrefs) string {
	if r := strings.TrimSpace(in.Runner); r != "" {
		return r
	}
	return p.StageRunner(routeStage(in.Stage))
}

// routeStage is the pipeline stage whose routing (runner, panel, policy) a
// stage follows: a review-only item's branch_review follows review.
func routeStage(stage string) string {
	if stage == StageBranchReview {
		return StageReview
	}
	return stage
}

// phaseForStage is the item status a run's stage corresponds to: a plan
// run IS the item's planning phase, an implement/review/qa run its
// implementing phase. The item moves in on claim so a mid-run failure
// blocks from a valid edge (review H1).
func phaseForStage(stage string) string {
	switch stage {
	case StagePlan, StageCompare, StageSynthesize:
		return "planning"
	case StageImplement, StageReview, StageQA, StageBranchReview:
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
	// Presence fields are client strings: bound them.
	if in.Version = strings.TrimSpace(in.Version); len(in.Version) > 64 {
		in.Version = in.Version[:64]
	}
	if in.PollMS < 0 || in.PollMS > int64(24*time.Hour/time.Millisecond) {
		in.PollMS = 0
	}
	offered := map[string]bool{}
	for _, ws := range in.Workspaces {
		if err := auth.RequireWorkspaceQuiet(ctx, ws); err != nil {
			return nil, fmt.Errorf("%w: workspace scope", ErrNotFound)
		}
		offered[ws] = true
	}
	// Health is a client claim about the offered workspaces: bound it.
	for ws, reason := range in.Unhealthy {
		reason = strings.TrimSpace(reason)
		switch {
		case !offered[ws]:
			delete(in.Unhealthy, ws)
			continue
		case reason == "":
			reason = "preflight failed"
		case len(reason) > 500:
			reason = reason[:500]
		}
		in.Unhealthy[ws] = reason
	}
	// Indexable is a client claim about the offered workspaces too.
	indexable := in.Indexable[:0:0]
	for _, ws := range in.Indexable {
		if offered[ws] && !slices.Contains(indexable, ws) {
			indexable = append(indexable, ws)
		}
	}
	sort.Strings(indexable)
	in.Indexable = indexable
	// What the worker says about its runners is the input to every policy
	// decision: bounded like any client claim, and a change is an audit
	// event (a worker that starts reporting another provider or endpoint
	// for a runner must show in the trail).
	if len(in.FleetRev) > 64 {
		in.FleetRev = in.FleetRev[:64]
	}
	if len(in.Features) > 16 {
		in.Features = in.Features[:16]
	}
	in.RunnerFacts = sanitizeFacts(in.RunnerFacts)
	in.Executors = sanitizeExecutors(in.Executors, offered)
	if s.audit != nil {
		before, known := s.storage.WorkerFacts(ctx, in.WorkerID)
		defer func() {
			fctx := context.WithoutCancel(ctx)
			if after, _ := s.storage.WorkerFacts(fctx, in.WorkerID); after != before {
				s.recordFacts(fctx, in.WorkerID, before, after, known)
			}
		}()
	}
	if s.healthNotify != nil {
		// What the registry held before and after this poll: a row this
		// key may not write (another key's worker id) never changes, so it
		// never notifies.
		before := s.storage.WorkerUnhealthy(ctx, in.WorkerID)
		defer func() {
			hctx := context.WithoutCancel(ctx)
			s.healthChanged(hctx, in.WorkerID, before, s.storage.WorkerUnhealthy(hctx, in.WorkerID))
		}()
	}
	// A worker from before workspace config reads none of it: it would run
	// a managed workspace from its own file while the run recorded the
	// server's revision. It gets none of those workspaces' runs until it is
	// upgraded, and shows as unhealthy for them with that reason.
	if in.ServerCommands == nil && s.configRev != nil {
		for _, ws := range in.Workspaces {
			if _, set := in.Unhealthy[ws]; set || s.configRev(ctx, ws) == "" {
				continue
			}
			if in.Unhealthy == nil {
				in.Unhealthy = map[string]string{}
			}
			in.Unhealthy[ws] = "this worker's release cannot read the workspace's config from the server: upgrade it"
		}
	}
	// The same for a workspace whose MCP servers the server holds: a
	// worker that cannot take them from the claim answer would give its
	// runs whatever its own mcp_config names.
	takesMCP := slices.Contains(in.Features, FeatureRunMCP)
	if s.mcpManaged != nil && !takesMCP {
		for _, ws := range in.Workspaces {
			if _, set := in.Unhealthy[ws]; set || !s.mcpManaged(ctx, ws) {
				continue
			}
			if in.Unhealthy == nil {
				in.Unhealthy = map[string]string{}
			}
			in.Unhealthy[ws] = "this worker's release cannot take the workspace's MCP servers from the server: upgrade it"
		}
	}
	// The worker's own interrupted runs may sit in a workspace this claim
	// does not offer. The same checks apply there as for an offered one:
	// the workspace is in this key's scope (the worker id is only the
	// caller's word), and the worker's release can do what it needs.
	in.mayTake = func(ws string) bool {
		if offered[ws] {
			return true // checked above; a refusal is in in.Unhealthy
		}
		if auth.RequireWorkspaceQuiet(ctx, ws) != nil {
			return false
		}
		if in.ServerCommands == nil && s.configRev != nil && s.configRev(ctx, ws) != "" {
			return false
		}
		return takesMCP || s.mcpManaged == nil || !s.mcpManaged(ctx, ws)
	}
	// Routing of the offered workspaces, resolved once per claim.
	prefs := make(map[string]*WorkspacePrefs, len(in.Workspaces))
	for _, ws := range in.Workspaces {
		prefs[ws] = s.prefsOf(ctx, ws)
	}
	// The worker's own runs may sit in a workspace this claim does not
	// offer: their routing and policy are resolved on demand, never
	// assumed absent.
	prefsFor := func(ws string) *WorkspacePrefs {
		if p, ok := prefs[ws]; ok {
			return p
		}
		p := s.prefsOf(ctx, ws)
		prefs[ws] = p
		return p
	}
	run, err := s.storage.Claim(ctx, in, prefsFor, s.lease, s.now().UTC())
	if err != nil || run == nil {
		return run, err
	}
	// The config the run executes under is named now and never changes for
	// this attempt: the worker fetches exactly this revision. A workspace
	// whose config cannot be resolved gets a revision nothing can fetch, so
	// the worker gives the run back rather than run it with other settings.
	if s.configRev != nil {
		rev := s.configRev(ctx, run.WorkspaceID)
		if err := s.storage.SetConfigRev(context.WithoutCancel(ctx), run.ID, run.Attempt, rev); err != nil {
			// Not recorded: the run must not execute under a revision its
			// record does not name.
			slog.Warn("runs: the claim's config revision was not recorded; the worker is given one it cannot fetch", "run", run.ID, "err", err)
			rev = UnreadableConfigRev
		}
		run.ConfigRev = rev
	}
	// The run's MCP servers, with their credentials: in this answer only,
	// for this run's workspace only, never stored. When they cannot be
	// resolved the worker is told so and gives the run back — it does not
	// run without them, nor with its own.
	if s.runMCP != nil {
		// Credentials are resolved only for a workspace in the claiming
		// key's scope — the claim scans keep to that; this is the last
		// check before a secret leaves the server.
		if auth.RequireWorkspaceQuiet(ctx, run.WorkspaceID) != nil {
			slog.Error("runs: a run was claimed for a workspace outside the claiming key's scope; it is released and nothing is handed out", "run", run.ID, "workspace", run.WorkspaceID)
			if err := s.storage.ReleaseClaim(context.WithoutCancel(ctx), run.ID, run.Attempt, s.now().UTC()); err != nil {
				return nil, err
			}
			return nil, nil
		}
		mcp, err := s.runMCP(context.WithoutCancel(ctx), run.WorkspaceID, in.ServerCommands != nil && *in.ServerCommands)
		if err != nil {
			slog.Warn("runs: the run's MCP servers could not be resolved; the worker is told to give the run back", "run", run.ID, "err", err)
			mcp = &RunMCP{Unreadable: true}
		}
		// The workspace's config came to name MCP servers between the
		// check above and now, and this worker's release cannot take them
		// from the answer: the run goes to a worker that can.
		if mcp != nil && !takesMCP {
			if err := s.storage.ReleaseClaim(context.WithoutCancel(ctx), run.ID, run.Attempt, s.now().UTC()); err != nil {
				return nil, err
			}
			return nil, nil
		}
		run.MCP = mcp
	}
	// The claim has committed: what follows must not depend on the worker
	// still waiting for the response (a client timeout used to leave the item
	// approved while its run sat claimed).
	ctx = context.WithoutCancel(ctx)
	// The item enters the run's phase on claim (H1). Re-claims of an item
	// already in phase fail the matrix quietly — that is fine.
	if to := phaseForStage(run.Stage); to != "" && s.itemTransition != nil {
		_ = s.itemTransition(ctx, run.ItemID, to, "run claimed: "+run.ID, "worker:"+run.WorkerID)
	}
	// A plan group member: the item now plans, so its plan can be created
	// and linked (idempotent — the first member does it).
	if run.GroupID != "" && run.Stage == StagePlan && s.ensurePlan != nil {
		if err := s.ensurePlan(ctx, run.ItemID); err != nil {
			s.chainNote(ctx, run, "ensuring the plan failed: "+err.Error())
		}
	}
	return run, nil
}

// SetWorkerHealthNotifier wires the hook told when a worker turns unhealthy
// for a workspace or recovers.
func (s *Service) SetWorkerHealthNotifier(fn func(ctx context.Context, h WorkerHealth)) {
	s.healthNotify = fn
}

// healthChanged tells the health notifier what changed between the stored
// health and the claim's: each newly failing workspace, each recovered one.
// A reason that changes while still failing is not news.
func (s *Service) healthChanged(ctx context.Context, worker string, before, after map[string]string) {
	for ws, reason := range after {
		if before[ws] == "" {
			slog.Warn("worker unhealthy for a workspace", "worker", worker, "workspace", ws, "reason", reason)
			s.healthNotify(ctx, WorkerHealth{Worker: worker, Workspace: ws, Reason: reason})
		}
	}
	for ws := range before {
		if after[ws] == "" {
			slog.Info("worker healthy again", "worker", worker, "workspace", ws)
			s.healthNotify(ctx, WorkerHealth{Worker: worker, Workspace: ws, Healthy: true})
		}
	}
}

// QueueSeeded queues one plan run that re-plans from a candidate (revise
// at a multi-agent gate): never a group, pinned to the candidate's runner,
// the seed revision carried in plan_rev_id.
func (s *Service) QueueSeeded(ctx context.Context, itemID, workspaceID, stage, seedRevisionID, runner string) (string, error) {
	r, err := s.Queue(ctx, QueueInput{WorkspaceID: workspaceID, ItemID: itemID, Stage: stage, PlanRevID: seedRevisionID, Runner: runner, Single: true})
	if err != nil {
		return "", err
	}
	return r.ID, nil
}

// PinGroupBase records a plan group's base commit once (the first member to
// start) and returns the pinned base every member must plan at.
func (s *Service) PinGroupBase(ctx context.Context, runID, baseSHA string) (string, error) {
	if err := s.checkScope(ctx, runID); err != nil {
		return "", err
	}
	r, err := s.storage.Get(ctx, strings.TrimSpace(runID))
	if err != nil {
		return "", err
	}
	if r.GroupID == "" {
		return strings.TrimSpace(baseSHA), nil // not grouped: nothing to pin
	}
	if strings.TrimSpace(baseSHA) == "" {
		return "", fmt.Errorf("%w: base_sha is required", ErrInvalidInput)
	}
	return s.storage.SetGroupBase(ctx, r.GroupID, strings.TrimSpace(baseSHA))
}

// Heartbeat extends the worker's lease; the first flips claimed -> running.
// A non-empty phase says what a running implement run is doing.
func (s *Service) Heartbeat(ctx context.Context, id, workerID, leaseToken, phase string) error {
	id = strings.TrimSpace(id)
	if id == "" || strings.TrimSpace(workerID) == "" {
		return fmt.Errorf("%w: id and worker_id are required", ErrInvalidInput)
	}
	if phase != "" && phase != PhaseChecks && phase != PhaseFixing && phase != PhaseReviewing {
		return fmt.Errorf("%w: unknown phase %q", ErrInvalidInput, phase)
	}
	if err := s.checkScope(ctx, id); err != nil {
		return err
	}
	keyID := callerKeyID(ctx)
	if err := s.storage.Heartbeat(ctx, id, workerID, keyID, hashLeaseToken(leaseToken), phase, s.lease, s.now().UTC()); err != nil {
		return err
	}
	s.storage.TouchWorker(ctx, workerID, keyID, s.now().UTC())
	return nil
}

// WorkerOffline records a worker's clean-shutdown goodbye. Only the key
// that owns the registry row may say it (others get not-found).
func (s *Service) WorkerOffline(ctx context.Context, workerID string) error {
	workerID = strings.TrimSpace(workerID)
	if workerID == "" {
		return fmt.Errorf("%w: worker id is required", ErrInvalidInput)
	}
	ok, err := s.storage.WorkerOffline(ctx, workerID, callerKeyID(ctx), s.now().UTC())
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: worker %s", ErrNotFound, workerID)
	}
	return nil
}

// Presence windows: an idle worker is online while it polled within three
// poll intervals (at least idleWindowMin); a busy one sends heartbeats and
// counts as online for busyWindow.
const (
	idleWindowMin = 2 * time.Minute
	busyWindow    = 15 * time.Minute
)

// workerState: offline after a goodbye or a long silence; busy while it
// holds a claimed/running run; paused when an online worker holds none and
// a pause is in force; idle otherwise.
func workerState(w WorkerSeen, busy bool, now time.Time) string {
	seen, err := time.Parse(time.RFC3339Nano, w.LastSeen)
	if err != nil {
		return "offline"
	}
	if w.OfflineAt != "" && w.OfflineAt >= w.LastSeen {
		return "offline"
	}
	idle := 3 * time.Duration(w.PollMS) * time.Millisecond
	if idle < idleWindowMin {
		idle = idleWindowMin
	}
	switch {
	case busy && now.Sub(seen) <= busyWindow:
		return "busy"
	case now.Sub(seen) <= idle && w.Paused != nil:
		return "paused"
	case now.Sub(seen) <= idle:
		return "idle"
	}
	return "offline"
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
	// The executor id and the grants hash are short worker-chosen labels
	// ("local", "wrapper:<name>:<12 hex>"); bound them like any client
	// string.
	if in.Executor = strings.TrimSpace(in.Executor); len(in.Executor) > 200 {
		in.Executor = in.Executor[:200]
	}
	if in.Grants = strings.TrimSpace(in.Grants); len(in.Grants) > 64 {
		in.Grants = in.Grants[:64]
	}
	mismatch := s.checkExecutor(ctx, id, &in)
	// The dashboard renders pr_url as a link: http(s) only, bounded.
	if in.PRURL = strings.TrimSpace(in.PRURL); len(in.PRURL) > 512 ||
		(!strings.HasPrefix(in.PRURL, "https://") && !strings.HasPrefix(in.PRURL, "http://")) {
		in.PRURL = ""
	}
	if in.Status == StatusFailed && in.Retryable {
		run, requeued, err := s.retry(ctx, id, in, leaseToken)
		if err != nil || requeued {
			if err == nil {
				s.recordMismatch(ctx, run, mismatch)
			}
			return run, err
		}
		in.Reason = fmt.Sprintf("failed after %d attempts: %s", maxRetries+1, in.Reason)
	}
	run, err := s.storage.Report(ctx, id, in.WorkerID, callerKeyID(ctx), hashLeaseToken(leaseToken), in, s.now().UTC())
	if err != nil {
		return nil, err
	}
	s.recordMismatch(ctx, run, mismatch)
	s.chain(ctx, run, in)
	return run, nil
}

// retry sends a retryably failed run back to the queue while it has
// retries left (requeued = true): the item stays where it is and nobody is
// told — the worker's problem is not the approver's. Spent retries fall through
// to the ordinary failure (Report + chain → blocked).
func (s *Service) retry(ctx context.Context, id string, in ReportInput, leaseToken string) (run *Run, requeued bool, err error) {
	cur, err := s.storage.Get(ctx, id)
	if err != nil {
		return nil, false, err
	}
	if cur.Retries >= maxRetries {
		return nil, false, nil
	}
	now := s.now().UTC()
	avoid := ""
	if cur.Retries >= 1 {
		// The second retry gives another worker the first go.
		avoid = in.WorkerID
	}
	note := fmt.Sprintf("attempt %d of %d failed on %s, retrying: %s", cur.Retries+1, maxRetries+1, in.WorkerID, firstLine(in.Reason, in.Summary))
	run, err = s.storage.Requeue(ctx, id, in.WorkerID, callerKeyID(ctx), hashLeaseToken(leaseToken), in, note, now.Add(retryBackoff[cur.Retries]), avoid, now)
	if err != nil {
		return nil, false, err
	}
	slog.Info("run requeued after a retryable failure", "run", id, "item", run.ItemID, "stage", run.Stage, "worker", in.WorkerID, "retry", run.Retries)
	return run, true, nil
}

// chain applies the item-side consequence of a finished run. Item-move
// failures are recorded on the run (the worker's report stays truthful
// about the stage; the chain outcome lands in fail_reason for diagnosis).
func (s *Service) chain(ctx context.Context, run *Run, in ReportInput) {
	// A second opinion only adds a section; it never moves the item.
	if run.Stage == StageSecondOpinion {
		s.secondOpinionDone(ctx, run, in)
		return
	}
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
	// Advisory plan stages: success or failure, the plan round moves on
	// (a failed judge or merge never blocks the candidates).
	if run.Stage == StageCompare || run.Stage == StageSynthesize {
		if run.Status == StatusFailed {
			s.chainNote(ctx, run, run.Stage+" failed (advisory): "+firstLine(in.Reason, in.Summary))
		}
		s.afterAdvisory(ctx, run.Stage, run.ItemID, run.WorkspaceID, "worker:"+run.WorkerID)
		return
	}
	switch run.Status {
	case StatusFailed:
		move("blocked", "run failed: "+firstLine(in.Reason, in.Summary))
	case StatusSucceeded:
		switch run.Stage {
		case StagePlan:
			// The worker wrote the revision and enrichment. The claim
			// normally moved the item into planning already; only when that
			// move did not happen is it still queued, and then the matrix
			// walk is queued -> planning -> awaiting_approval. Trying the
			// gate first keeps a false "move failed" note off a good run.
			agent := "worker:" + run.WorkerID
			if s.itemTransition(ctx, run.ItemID, "awaiting_approval", "", agent) != nil {
				move("planning", "")
				move("awaiting_approval", "")
			}
		case StageImplement, StageReview, StageQA:
			s.advance(ctx, run, "worker:"+run.WorkerID)
		case StageBranchReview:
			// The verdict is the deliverable: pass or fail, the approver decides.
			s.toReview(ctx, run.ItemID, "worker:"+run.WorkerID)
		}
	}
}

// toReview hands a review-only item to the approver (implementing → in_review;
// the claim normally made the first move already).
func (s *Service) toReview(ctx context.Context, itemID, agent string) {
	_ = s.itemTransition(ctx, itemID, "implementing", "", agent)
	if err := s.itemTransition(ctx, itemID, "in_review", "", agent); err != nil {
		slog.Warn("moving a reviewed branch to in_review", "item", itemID, "err", err)
	}
}

// advance continues after a successful implement/review/qa stage (a single
// run, or a group's passing decision): queue the next configured stage, or
// hand the item to the approver at in_review.
func (s *Service) advance(ctx context.Context, run *Run, agentID string) {
	next := s.nextStage(run.WorkspaceID, run.Stage)
	// Review/QA run only when the workspace configures them (one runner or
	// a group); otherwise the sequence ends and the approver reviews.
	if next != "" && s.prefsOf(ctx, run.WorkspaceID).Configured(next) {
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
		if runs[i].Stage == StageSecondOpinion { // advisory: resumes on its own, never with an amendment
			continue
		}
		_ = s.storage.Resume(ctx, runs[i].ID, runs[i].WorkerID, s.lease, s.now().UTC())
	}
	return nil
}

// ListWorkers is the worker registry (capabilities recorded at claim).
// Root and "*" keys see every worker; a scoped key sees only workers that
// serve at least one of its workspaces, with the workspace list cut to its
// scope — never another tenant's workers or workspace ids (review H4).
func (s *Service) ListWorkers(ctx context.Context) ([]WorkerSeen, error) {
	all, err := s.storage.ListWorkers(ctx)
	if err != nil {
		return nil, err
	}
	busy, err := s.storage.BusyWorkers(ctx)
	if err != nil {
		return nil, err
	}
	now := s.now().UTC()
	for i := range all {
		if !all[i].Paused.activeAt(now) {
			all[i].Paused = nil
		}
		all[i].State = workerState(all[i], busy[all[i].ID], now)
	}
	p := auth.PrincipalFromContext(ctx)
	if p.IsRoot() || p.AllWorkspaces {
		return all, nil
	}
	out := make([]WorkerSeen, 0, len(all))
	for _, w := range all {
		var mine []string
		for _, ws := range w.Workspaces {
			if p.CanAccess(ws) {
				mine = append(mine, ws)
			}
		}
		if len(mine) == 0 {
			continue
		}
		w.Workspaces = mine
		for ws := range w.Unhealthy {
			if !p.CanAccess(ws) {
				delete(w.Unhealthy, ws)
			}
		}
		w.Indexable = slices.DeleteFunc(w.Indexable, func(ws string) bool { return !p.CanAccess(ws) })
		for ws := range w.Executors {
			if !p.CanAccess(ws) {
				delete(w.Executors, ws)
			}
		}
		out = append(out, w)
	}
	return out, nil
}

// RunSummary is the inbox-facing slice of a run — enough for the Slack
// relay's gate messages and the `done` command's head-sha lookup.
type RunSummary struct {
	ID       string `json:"id"`
	Stage    string `json:"stage"`
	Status   string `json:"status"`
	Runner   string `json:"runner,omitempty"`
	Branch   string `json:"branch,omitempty"`
	HeadSHA  string `json:"head_sha,omitempty"`
	Diffstat string `json:"diffstat,omitempty"`
	Summary  string `json:"result_summary,omitempty"`
	// FailReason: why a failed run failed (what a Blocked notice quotes).
	FailReason string `json:"fail_reason,omitempty"`
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
		out = append(out, RunSummary{ID: r.ID, Stage: r.Stage, Status: string(r.Status), Runner: r.Runner,
			Branch: r.Branch, HeadSHA: r.HeadSHA, Diffstat: r.Diffstat, Summary: r.ResultSummary, FailReason: r.FailReason, GroupID: r.GroupID})
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

// indexDutyWindow: a worker counts as able to index a workspace while its
// registry row (refreshed by its polls) is this recent.
const indexDutyWindow = 15 * time.Minute

// IndexDuty answers a claiming worker what to index while idle (design §3):
// of its offered workspaces, those with the code_index switch on, and of
// those the ones whose default branch it keeps fresh — the idle able worker
// with the smallest id. A busy worker cannot index (it indexes only while
// idle), so the duty moves off it for the length of its run.
func (s *Service) IndexDuty(ctx context.Context, in ClaimInput) IndexDuty {
	var duty IndexDuty
	for _, ws := range in.Workspaces {
		if auth.RequireWorkspaceQuiet(ctx, ws) != nil || !s.prefsOf(ctx, ws).CodeIndexOn() {
			continue
		}
		duty.Enabled = append(duty.Enabled, ws)
		if !slices.Contains(in.Indexable, ws) {
			continue
		}
		able, err := s.storage.IndexableWorkers(ctx, ws, s.now().Add(-indexDutyWindow), s.now())
		if err != nil {
			continue
		}
		// The claiming worker is idle right now (it is polling) whatever its
		// last run's row says; the others count only when not busy.
		me := strings.TrimSpace(in.WorkerID)
		for _, a := range able {
			if a.ID == me || !a.Busy {
				if a.ID == me {
					duty.Default = append(duty.Default, ws)
				}
				break
			}
		}
	}
	return duty
}
