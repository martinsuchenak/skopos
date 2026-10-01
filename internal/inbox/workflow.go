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
	if class == ActorHuman {
		if err := auth.RequireApprover(ctx); err != nil {
			return nil, err
		}
	}
	if err := s.requireItemScopeQuiet(ctx, itemID); err != nil {
		return nil, err
	}
	itemID = strings.TrimSpace(itemID)
	err := s.store.RunInTx(ctx, func(tx Store) error {
		item, err := tx.GetItem(ctx, itemID)
		if err != nil {
			return err
		}
		actor, ok := WorkflowTransition(item.Status, to)
		if !ok {
			return fmt.Errorf("%w: %s is not a workflow transition from %s to %s", ErrInvalidInput, statusLabel(item.Status), item.Status, to)
		}
		if actor != class {
			return fmt.Errorf("%w: transition %s → %s belongs to the %s (this call is %s)", ErrInvalidInput, item.Status, to, actor, class)
		}
		return tx.SetStatus(ctx, itemID, to, s.now().UTC())
	})
	if err != nil {
		return nil, err
	}
	s.publishItem(ctx, itemID)
	s.auditItem(ctx, itemID, action, via, notes, agentID)
	return s.store.GetItem(ctx, itemID)
}

// --- human actions (approver-gated; agent and worker keys never pass) ---

// Queue moves an open item into the agent workflow (R1).
func (s *Service) Queue(ctx context.Context, itemID, via string) (*Item, error) {
	return s.transition(ctx, itemID, StatusQueued, ActorHuman, "inbox.queue", via, "", "")
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
	return s.transition(ctx, item.ID, StatusApproved, ActorHuman, "inbox.approve", input.Via, input.Notes, "")
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
	var target Status
	switch item.Status {
	case StatusAwaitingApproval:
		target = StatusPlanning
	case StatusInReview:
		target = StatusImplementing
	default:
		return nil, fmt.Errorf("%w: request-changes applies to awaiting_approval or in_review items (item is %s)", ErrInvalidInput, statusLabel(item.Status))
	}
	return s.transition(ctx, item.ID, target, ActorHuman, "inbox.request_changes", via, notes, "")
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
	target := StatusPlanning
	if item.PlanID != "" && s.plans != nil {
		if rev, revErr := s.plans.LatestRevision(ctx, item.PlanID); revErr == nil && rev != nil && rev.LockedAt != nil {
			target = StatusImplementing
		}
	}
	return s.transition(ctx, item.ID, target, ActorHuman, "inbox.retry", via, answer, "")
}

// Reject discards the item from any workflow phase that allows it.
func (s *Service) Reject(ctx context.Context, itemID, notes, via string) (*Item, error) {
	return s.transition(ctx, itemID, StatusDiscarded, ActorHuman, "inbox.reject", via, notes, "")
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
		if items[i].Status == StatusAwaitingApproval {
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
