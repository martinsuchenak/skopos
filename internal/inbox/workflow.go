package inbox

import (
	"context"
	"fmt"
	"strings"

	"github.com/martinsuchenak/skopos/internal/approvals"
	"github.com/martinsuchenak/skopos/internal/audit"
	"github.com/martinsuchenak/skopos/internal/auth"
	"github.com/martinsuchenak/skopos/internal/plans"
)

// The agent-pipeline workflow on inbox items (docs/design/agent-pipeline.md
// §1). Statuses and the transition matrix live in models.go; this file adds
// the human actions (approver-gated), the system-transition path for the
// executor, and the audit-logged timeline view.

// AuditRecorder is the audit sink; internal/audit.Service satisfies it.
type AuditRecorder interface {
	Record(ctx context.Context, input audit.RecordInput) error
	Timeline(ctx context.Context, entityType, entityID string) ([]audit.Entry, error)
}

// PlanRevisions gives the workflow actions access to plan revisions (the
// plans service satisfies it); used to bind approvals and lock them.
type PlanRevisions interface {
	LatestRevision(ctx context.Context, planID string) (*plans.Revision, error)
	LockRevision(ctx context.Context, revisionID string) error
	VerifyRevisionCurrent(ctx context.Context, planID string) (*plans.Revision, error)
	AmendRevision(ctx context.Context, planID, baseSHA string) (*plans.Revision, error)
	CreateRevision(ctx context.Context, planID, baseSHA string) (*plans.Revision, error)
}

// ApprovalLog records human decisions; internal/approvals.Service satisfies it.
type ApprovalLog interface {
	Record(ctx context.Context, input approvals.RecordInput) (approvals.Entry, error)
	List(ctx context.Context, itemID string) ([]approvals.Entry, error)
}

// RunQueuer is the run-queue hook (internal/runs.Service satisfies it):
// queueing an item queues its plan run; approving queues the implement run
// bound to the approved revision and base; request-changes and retry queue
// the stage they return the item to; reject/done cancel the item's active
// runs; approving an amendment resumes the paused run it froze. Nil
// disables (pure 1a mode).
type RunQueuer interface {
	QueueStage(ctx context.Context, itemID, workspaceID, stage, planRevID, baseSHA, branch string) (string, error)
	CancelRun(ctx context.Context, runID, reason string) error
	CancelItem(ctx context.Context, itemID, reason string) error
	ResumeItem(ctx context.Context, itemID string) error
}

const auditEntityItem = "inbox_item"

// auditItem records one transition, best-effort: an audit failure never
// blocks the mutation that produced it (plan 01a0bf7c).
func (s *Service) auditItem(ctx context.Context, itemID, action, via, notes, agentID string) {
	if s.auditSink == nil {
		return
	}
	ws, err := s.store.ItemWorkspace(ctx, itemID)
	if err != nil {
		return
	}
	_ = s.auditSink.Record(ctx, audit.RecordInput{
		WorkspaceID: ws,
		EntityType:  auditEntityItem,
		EntityID:    itemID,
		Action:      action,
		Via:         via,
		Notes:       notes,
		AgentID:     agentID,
	})
}

// transition is the shared engine behind every workflow move: scope check,
// matrix check (from → to for this actor class), then the write, event, and
// audit record. Human transitions require the approver permission; system
// transitions only require workspace scope (the executor's key).
func (s *Service) transition(ctx context.Context, itemID string, to Status, class TransitionActor, action, via, notes, agentID string) (*Item, error) {
	return s.transitionWith(ctx, itemID, to, class, action, via, notes, agentID, nil)
}

// appendNotes returns the in-transaction mutation that appends a markdown
// section of human notes to the item (nil when there are none) — the stage
// prompts read exactly these sections. Bounded like a retry answer.
func (s *Service) appendNotes(ctx context.Context, heading, notes string) (func(tx Store, cur *Item) error, error) {
	notes = strings.TrimSpace(notes)
	if notes == "" {
		return nil, nil
	}
	if len(notes) > maxAnswerLen {
		return nil, fmt.Errorf("%w: notes are %d bytes; the limit is %d", ErrInvalidInput, len(notes), maxAnswerLen)
	}
	return func(tx Store, cur *Item) error {
		content := cur.Content + "\n\n" + heading + "\n\n" + notes + "\n"
		return tx.UpdateItem(ctx, cur.ID, cur.WorkspaceID, cur.Title, content, tagsJSON(cur.Tags), cur.Priority, s.now().UTC())
	}, nil
}

// transitionWith is transition plus an optional in-transaction mutation of
// the freshly read item, run after the matrix check and before the status
// write: the mutation and the move commit together or not at all (review
// M6 — Retry's answer append).
func (s *Service) transitionWith(ctx context.Context, itemID string, to Status, class TransitionActor, action, via, notes, agentID string, mutate func(tx Store, item *Item) error) (*Item, error) {
	if class == ActorHuman {
		if err := auth.RequireApprover(ctx); err != nil {
			return nil, err
		}
	}
	if err := s.requireItemScopeQuiet(ctx, itemID); err != nil {
		return nil, err
	}
	itemID = strings.TrimSpace(itemID)
	from := Status("")
	err := s.store.RunInTx(ctx, func(tx Store) error {
		item, err := tx.GetItem(ctx, itemID)
		if err != nil {
			return err
		}
		from = item.Status
		actor, ok := WorkflowTransition(item.Status, to)
		if !ok {
			return fmt.Errorf("%w: %s is not a workflow transition from %s to %s", ErrInvalidInput, statusLabel(item.Status), item.Status, to)
		}
		if actor != class {
			return fmt.Errorf("%w: transition %s → %s belongs to the %s (this call is %s)", ErrInvalidInput, item.Status, to, actor, class)
		}
		if mutate != nil {
			if err := mutate(tx, item); err != nil {
				return err
			}
		}
		return tx.SetStatus(ctx, itemID, to, s.now().UTC())
	})
	if err != nil {
		return nil, err
	}
	s.publishItem(ctx, itemID)
	s.auditItem(ctx, itemID, action, via, notes, agentID)
	if s.itemNotifier != nil {
		s.itemNotifier.NotifyItem(ctx, itemID, from, to)
	}
	return s.store.GetItem(ctx, itemID)
}

// --- human actions (approver-gated; agent and worker keys never pass) ---

// Queue moves an open item into the agent workflow (R1) and queues its plan
// run. The run is queued BEFORE the status change and cancelled if the
// change fails, so a failure is cleanly retryable (review M3).
func (s *Service) Queue(ctx context.Context, itemID, via string) (*Item, error) {
	// N1: authorize and matrix-validate BEFORE any side effect — queueing
	// first let a non-approver mint runs whose transition then failed.
	if err := auth.RequireApprover(ctx); err != nil {
		return nil, err
	}
	if err := s.requireItemScopeQuiet(ctx, itemID); err != nil {
		return nil, err
	}
	cur, err := s.store.GetItem(ctx, strings.TrimSpace(itemID))
	if err != nil {
		return nil, err
	}
	if _, ok := WorkflowTransition(cur.Status, StatusQueued); !ok {
		return nil, fmt.Errorf("%w: %s cannot be queued (item is %s)", ErrInvalidInput, statusLabel(cur.Status), statusLabel(cur.Status))
	}
	var queuedRun string
	if s.runQueuer != nil {
		id, qErr := s.runQueuer.QueueStage(ctx, cur.ID, cur.WorkspaceID, "plan", "", "", "")
		if qErr != nil {
			return nil, qErr
		}
		queuedRun = id
	}
	item, err := s.transition(ctx, itemID, StatusQueued, ActorHuman, "inbox.queue", via, "", "")
	if err != nil && s.runQueuer != nil && queuedRun != "" {
		// Targeted compensation (N1): cancel only the run this call made.
		_ = s.runQueuer.CancelRun(ctx, queuedRun, "queue transition failed")
	}
	return item, err
}

// ApproveInput binds an approval to exactly what Martin saw (review fix 3):
// the revision id his client rendered, and optionally its content hash.
type ApproveInput struct {
	Notes       string
	Via         string
	RevisionID  string
	ContentHash string
}

// Approve records Martin's plan approval (awaiting_approval → approved),
// bound to the plan revision he actually saw. The item must link a plan with
// a revision (no silent ungated approvals — R3); the named revision must
// still be the latest, and its snapshot must still match the plan's current
// steps (nothing slipped in after the screenshot). On success the revision
// locks, the approval names it plus its base, and the item moves.
func (s *Service) Approve(ctx context.Context, itemID string, input ApproveInput) (*Item, error) {
	if err := auth.RequireApprover(ctx); err != nil {
		return nil, err
	}
	if err := s.requireItemScopeQuiet(ctx, itemID); err != nil {
		return nil, err
	}
	item, err := s.store.GetItem(ctx, strings.TrimSpace(itemID))
	if err != nil {
		return nil, err
	}
	if item.PlanID == "" {
		return nil, fmt.Errorf("%w: item has no linked plan — nothing to approve", ErrInvalidInput)
	}
	if s.plans == nil {
		return nil, fmt.Errorf("%w: plan revisions are not configured", ErrInvalidInput)
	}
	if item.Status != StatusAwaitingApproval {
		return nil, fmt.Errorf("%w: only awaiting_approval items can be approved (item is %s)", ErrInvalidInput, statusLabel(item.Status))
	}
	rev, err := s.plans.LatestRevision(ctx, item.PlanID)
	if err != nil {
		return nil, fmt.Errorf("%w: item has no plan revision — the planner must snapshot before approval", ErrInvalidInput)
	}
	if input.RevisionID != "" && input.RevisionID != rev.ID {
		return nil, ErrStaleRevision
	}
	if input.ContentHash != "" && input.ContentHash != rev.ContentHash {
		return nil, ErrStaleRevision
	}
	// The snapshot must still describe the plan as it stands: steps edited
	// after the snapshot make the approval stale (re-show, re-approve).
	verified, err := s.plans.VerifyRevisionCurrent(ctx, item.PlanID)
	if err != nil {
		return nil, err
	}
	// Order matters (review fix 3): every gate's bookkeeping happens BEFORE
	// the status change — lock, approval record, then transition. A failure
	// at any earlier step leaves the item unapproved rather than approved
	// with nothing bound. The residual race (item mutated between the read
	// above and the transition's own tx) is caught by the transition's
	// matrix check.
	if err := s.plans.LockRevision(ctx, rev.ID); err != nil {
		return nil, err
	}
	s.auditItem(ctx, item.ID, "plan.lock_revision", input.Via, rev.ID, "")
	if s.approvals != nil {
		if _, err := s.approvals.Record(ctx, approvals.RecordInput{
			WorkspaceID: item.WorkspaceID,
			ItemID:      item.ID,
			Gate:        approvals.GatePlan,
			Subject:     rev.ID + "+" + rev.BaseSHA,
			Decision:    approvals.DecisionApproved,
			Via:         input.Via,
			Notes:       input.Notes,
		}); err != nil {
			return nil, fmt.Errorf("recording approval: %w", err)
		}
	}
	_ = verified
	// Queue the implement run bound to the approved revision and its base
	// (review H3) BEFORE the status change; a paused run (an approved
	// amendment) is resumed instead (H4).
	var queuedRun string
	if s.runQueuer != nil {
		if s.hasPausedRun(ctx, item.ID) {
			if rErr := s.runQueuer.ResumeItem(ctx, item.ID); rErr != nil {
				return nil, rErr
			}
		} else {
			id, qErr := s.runQueuer.QueueStage(ctx, item.ID, item.WorkspaceID, "implement", rev.ID, rev.BaseSHA, "")
			if qErr != nil {
				return nil, qErr
			}
			queuedRun = id
		}
	}
	// Approval notes (answers to the planner's questions, overrides) are
	// for the implementer: appended to the item, which its prompt reads,
	// with the status change — every surface, not just the Slack relay.
	appendApproval, nerr := s.appendNotes(ctx, "## Approval notes", input.Notes)
	if nerr != nil {
		return nil, nerr
	}
	item, terr := s.transitionWith(ctx, item.ID, StatusApproved, ActorHuman, "inbox.approve", input.Via, input.Notes, "", appendApproval)
	if terr != nil && s.runQueuer != nil && queuedRun != "" {
		_ = s.runQueuer.CancelRun(ctx, queuedRun, "approve transition failed")
	}
	if terr != nil {
		return nil, terr
	}
	return item, nil
}

// RequestChanges sends the item back one phase with Martin's notes. The
// target is picked from the current status inside the matrix (review fix 6):
// awaiting_approval → planning, in_review → implementing. The notes are
// passed into the next run's prompt.
func (s *Service) RequestChanges(ctx context.Context, itemID, notes, via string) (*Item, error) {
	if err := s.requireItemScopeQuiet(ctx, itemID); err != nil {
		return nil, err
	}
	item, err := s.store.GetItem(ctx, strings.TrimSpace(itemID))
	if err != nil {
		return nil, err
	}
	// N1: authorize and validate BEFORE queueing (a non-approver key used
	// to mint the run, then fail the transition, leaving it to run).
	if err := auth.RequireApprover(ctx); err != nil {
		return nil, err
	}
	var target Status
	stage := ""
	switch item.Status {
	case StatusAwaitingApproval:
		target, stage = StatusPlanning, "plan"
	case StatusInReview:
		target, stage = StatusImplementing, "implement"
	default:
		return nil, fmt.Errorf("%w: request-changes applies to awaiting_approval or in_review items (item is %s)", ErrInvalidInput, statusLabel(item.Status))
	}
	revID, baseSHA := s.approvedRevision(ctx, item)
	var queuedRun string
	if s.runQueuer != nil {
		id, qErr := s.runQueuer.QueueStage(ctx, item.ID, item.WorkspaceID, stage, revID, baseSHA, "")
		if qErr != nil {
			return nil, qErr
		}
		queuedRun = id
	}
	// The notes are why the work goes back: appended where the next run's
	// prompt reads them — "## Review notes" for a re-plan, "## Change
	// requests" for a re-implementation — atomically with the move.
	heading := "## Review notes"
	if target == StatusImplementing {
		heading = "## Change requests"
	}
	appendRequest, nerr := s.appendNotes(ctx, heading, notes)
	if nerr != nil {
		return nil, nerr
	}
	out, err := s.transitionWith(ctx, item.ID, target, ActorHuman, "inbox.request_changes", via, notes, "", appendRequest)
	if err != nil && s.runQueuer != nil && queuedRun != "" {
		_ = s.runQueuer.CancelRun(ctx, queuedRun, "request-changes transition failed")
	}
	return out, err
}

// approvedRevision returns the locked revision id and base for an item, when
// one exists (retry and change-request runs carry the approved binding).
func (s *Service) approvedRevision(ctx context.Context, item *Item) (string, string) {
	if item.PlanID == "" || s.plans == nil {
		return "", ""
	}
	rev, err := s.plans.LatestRevision(ctx, item.PlanID)
	if err != nil || rev == nil || rev.LockedAt == nil {
		return "", ""
	}
	return rev.ID, rev.BaseSHA
}

// Retry answers a blocked or failed item and returns it to the phase it was
// blocked from: implementing when a revision is locked (an approved plan
// exists), otherwise planning (review fix 5). The answer feeds the next
// run's prompt, like request-changes.
func (s *Service) Retry(ctx context.Context, itemID, answer, via string) (*Item, error) {
	if err := s.requireItemScopeQuiet(ctx, itemID); err != nil {
		return nil, err
	}
	item, err := s.store.GetItem(ctx, strings.TrimSpace(itemID))
	if err != nil {
		return nil, err
	}
	if err := auth.RequireApprover(ctx); err != nil {
		return nil, err
	}
	switch item.Status {
	case StatusBlocked, StatusFailed:
		// admissible
	default:
		return nil, fmt.Errorf("%w: retry applies to blocked or failed items (item is %s)", ErrInvalidInput, statusLabel(item.Status))
	}
	target := StatusPlanning
	stage := "plan"
	revID, baseSHA := "", ""
	if item.PlanID != "" && s.plans != nil {
		if rev, revErr := s.plans.LatestRevision(ctx, item.PlanID); revErr == nil && rev != nil && rev.LockedAt != nil {
			target, stage = StatusImplementing, "implement"
			revID, baseSHA = rev.ID, rev.BaseSHA
		}
	}
	// The answer feeds the next run: the planner (and implementer) prompt
	// reads the item CONTENT, so the answer is appended there — the audit
	// trail alone would leave the agent asking the same questions again.
	// The append runs inside the transition's transaction on the re-read
	// item (review M6): a concurrent retry loses the matrix check instead of
	// the answer, and a failed queue/transition leaves no orphaned text for
	// the next retry to duplicate.
	answer = strings.TrimSpace(answer)
	appendAnswer, err := s.appendNotes(ctx, "## Answers", answer)
	if err != nil {
		return nil, err
	}
	var queuedRun string
	if s.runQueuer != nil {
		id, qErr := s.runQueuer.QueueStage(ctx, item.ID, item.WorkspaceID, stage, revID, baseSHA, "")
		if qErr != nil {
			return nil, qErr
		}
		queuedRun = id
	}
	out, err := s.transitionWith(ctx, item.ID, target, ActorHuman, "inbox.retry", via, answer, "", appendAnswer)
	if err != nil && s.runQueuer != nil && queuedRun != "" {
		_ = s.runQueuer.CancelRun(ctx, queuedRun, "retry transition failed")
	}
	return out, err
}

// Reject discards the item from any workflow phase that allows it and
// cancels its active runs.
func (s *Service) Reject(ctx context.Context, itemID, notes, via string) (*Item, error) {
	item, err := s.transition(ctx, itemID, StatusDiscarded, ActorHuman, "inbox.reject", via, notes, "")
	if err != nil {
		return nil, err
	}
	if s.runQueuer != nil {
		_ = s.runQueuer.CancelItem(ctx, item.ID, "item rejected")
	}
	return item, nil
}

// MarkDone completes the reviewed item (in_review → done). headSHA is
// REQUIRED (review fix 4): the review approval names exactly the commit
// Martin looked at, and any later commit would not match it (stale by
// comparison). The executor always knows the head it pushed.
func (s *Service) MarkDone(ctx context.Context, itemID, headSHA, via string) (*Item, error) {
	headSHA = strings.TrimSpace(headSHA)
	if headSHA == "" {
		return nil, fmt.Errorf("%w: head_sha is required — the review approval names the commit being approved", ErrInvalidInput)
	}
	item, err := s.transition(ctx, itemID, StatusDone, ActorHuman, "inbox.mark_done", via, "", "")
	if err != nil {
		return nil, err
	}
	if s.approvals != nil {
		_, _ = s.approvals.Record(ctx, approvals.RecordInput{
			WorkspaceID: item.WorkspaceID,
			ItemID:      item.ID,
			Gate:        approvals.GateReview,
			Subject:     headSHA,
			Decision:    approvals.DecisionApproved,
			Via:         via,
		})
	}
	if s.runQueuer != nil {
		_ = s.runQueuer.CancelItem(ctx, item.ID, "item done")
	}
	return item, nil
}

// Approvals lists the item's recorded decisions, newest-first, scoped to the
// caller like the timeline.
func (s *Service) Approvals(ctx context.Context, itemID string) ([]approvals.Entry, error) {
	if err := s.requireItemScopeQuiet(ctx, itemID); err != nil {
		return nil, err
	}
	if s.approvals == nil {
		return nil, fmt.Errorf("%w: approvals are not configured", ErrInvalidInput)
	}
	entries, err := s.approvals.List(ctx, strings.TrimSpace(itemID))
	if err != nil {
		return nil, err
	}
	if entries == nil {
		entries = []approvals.Entry{}
	}
	return entries, nil
}

// --- system transitions (the executor's path; scope-checked, not approver-gated) ---

// LinkPlan attaches a plan to a workflow item without moving it to
// converted (the manual path's Convert stays untouched): the planner's plan
// becomes the item's plan while the item runs the workflow. The plan must
// live in the item's workspace.
func (s *Service) LinkPlan(ctx context.Context, itemID, planID string) (*Item, error) {
	if err := s.requireItemScopeQuiet(ctx, itemID); err != nil {
		return nil, err
	}
	itemID = strings.TrimSpace(itemID)
	planID = strings.TrimSpace(planID)
	if planID == "" {
		return nil, fmt.Errorf("%w: plan_id is required", ErrInvalidInput)
	}
	err := s.store.RunInTx(ctx, func(tx Store) error {
		item, err := tx.GetItem(ctx, itemID)
		if err != nil {
			return err
		}
		if !IsWorkflowStatus(item.Status) {
			return fmt.Errorf("%w: only workflow items can link a plan mid-flight (item is %s)", ErrInvalidInput, statusLabel(item.Status))
		}
		if item.PlanID != "" {
			return fmt.Errorf("%w (plan %s)", ErrAlreadyConverted, item.PlanID)
		}
		if item.WorkspaceID == "" {
			return fmt.Errorf("%w: file the item into a workspace first", ErrInvalidInput)
		}
		planWS, err := tx.PlanWorkspace(ctx, planID)
		if err != nil {
			return err
		}
		if err := auth.RequireWorkspaceQuiet(ctx, planWS); err != nil {
			return fmt.Errorf("%w: plan %s", ErrNotFound, planID)
		}
		if planWS != item.WorkspaceID {
			return fmt.Errorf("%w: plan %s belongs to a different workspace than the item", ErrInvalidInput, planID)
		}
		return tx.SetItemPlan(ctx, itemID, planID, s.now().UTC())
	})
	if err != nil {
		return nil, err
	}
	s.publishItem(ctx, itemID)
	s.auditItem(ctx, itemID, "inbox.link_plan", "", "plan "+planID, "")
	return s.store.GetItem(ctx, itemID)
}

// SystemTransition moves an item along the machine phases as a side effect
// of a run: queued → planning, planning → awaiting_approval, approved →
// implementing, implementing → in_review / awaiting_approval (amendment) /
// blocked / failed. agentID names the acting agent for the audit record.
func (s *Service) SystemTransition(ctx context.Context, itemID string, to Status, reason, agentID, via string) (*Item, error) {
	return s.transition(ctx, itemID, to, ActorSystem, "inbox.transition", via, reason, agentID)
}

// AmendPlan is the amendment path (review fix 2): pause the run
// (implementing → awaiting_approval), then snapshot the amended plan over
// the locked revision. Doing both here makes the item's return to the gate
// inseparable from unlocking the plan content.
func (s *Service) AmendPlan(ctx context.Context, itemID, baseSHA string) (*Item, error) {
	item, err := s.transition(ctx, itemID, StatusAwaitingApproval, ActorSystem, "inbox.transition", "worker", "amendment", "")
	if err != nil {
		return nil, err
	}
	if item.PlanID != "" && s.plans != nil {
		if _, err := s.plans.AmendRevision(ctx, item.PlanID, baseSHA); err != nil {
			return nil, err
		}
	}
	return item, nil
}

// RequireAmendable is the plans service's amendment guard: the linked item
// must be back at the gate (awaiting_approval) — only AmendPlan's
// system transition puts it there.
func (s *Service) RequireAmendable(planID string) error {
	items, err := s.store.ItemsByPlan(context.Background(), planID)
	if err != nil {
		return err
	}
	for i := range items {
		// implementing: an in-flight amendment (the worker's call);
		// awaiting_approval: a re-amendment at the gate.
		if items[i].Status == StatusImplementing || items[i].Status == StatusAwaitingApproval {
			return nil
		}
	}
	return plans.ErrRevisionLocked
}

// RequirePlannable is the plans service's replan guard: the linked item
// must be in its planning phase (nothing approved yet).
func (s *Service) RequirePlannable(planID string) error {
	items, err := s.store.ItemsByPlan(context.Background(), planID)
	if err != nil {
		return err
	}
	for i := range items {
		if items[i].Status == StatusPlanning {
			return nil
		}
	}
	return plans.ErrRevisionLocked
}

// PlanHasWorkflowItem is the plans service's deletion guard: refuse deleting
// a plan whose linked item still runs the workflow (its locked revisions
// are what approvals point at).
func (s *Service) PlanHasWorkflowItem(planID string) error {
	items, err := s.store.ItemsByPlan(context.Background(), planID)
	if err != nil {
		return err
	}
	for i := range items {
		if IsWorkflowStatus(items[i].Status) {
			return fmt.Errorf("%w: plan has an item in the agent workflow (%s) — finish or reject the item first", ErrInvalidInput, statusLabel(items[i].Status))
		}
	}
	return nil
}

// Timeline is the item's workflow history: the audit log filtered to this
// entity (docs/design/agent-pipeline.md §1), scoped to the caller.
func (s *Service) Timeline(ctx context.Context, itemID string) ([]audit.Entry, error) {
	if err := s.requireItemScopeQuiet(ctx, itemID); err != nil {
		return nil, err
	}
	if s.auditSink == nil {
		return nil, fmt.Errorf("%w: audit log is not configured", ErrInvalidInput)
	}
	entries, err := s.auditSink.Timeline(ctx, auditEntityItem, strings.TrimSpace(itemID))
	if err != nil {
		return nil, err
	}
	if entries == nil {
		entries = []audit.Entry{}
	}
	return entries, nil
}

// hasPausedRun reports whether the item has a run parked by an amendment —
// approving the amendment re-arms that run instead of queueing a new one.
func (s *Service) hasPausedRun(ctx context.Context, itemID string) bool {
	if s.runLister == nil {
		return false
	}
	runs, err := s.runLister.ListRuns(ctx, itemID)
	if err != nil {
		return false
	}
	for _, r := range runs {
		if r.Status == "paused" {
			return true
		}
	}
	return false
}
