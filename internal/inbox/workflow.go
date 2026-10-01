package inbox

import (
	"context"
	"fmt"
	"strings"

	"github.com/martinsuchenak/skopos/internal/audit"
	"github.com/martinsuchenak/skopos/internal/auth"
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

// Approve records Martin's plan approval (awaiting_approval → approved).
// Notes may carry overrides (wr, runner, tags) — plan revisions and the
// approvals table land with items 4–5.
func (s *Service) Approve(ctx context.Context, itemID, notes, via string) (*Item, error) {
	return s.transition(ctx, itemID, StatusApproved, ActorHuman, "inbox.approve", via, notes, "")
}

// RequestChanges sends the item back one phase with Martin's notes
// (awaiting_approval → planning, or in_review → implementing). The notes are
// passed into the next run's prompt.
func (s *Service) RequestChanges(ctx context.Context, itemID, notes, via string) (*Item, error) {
	return s.transition(ctx, itemID, StatusPlanning, ActorHuman, "inbox.request_changes", via, notes, "")
}

// RequestChangesInReview is RequestChanges from the review phase
// (in_review → implementing).
func (s *Service) RequestChangesInReview(ctx context.Context, itemID, notes, via string) (*Item, error) {
	return s.transition(ctx, itemID, StatusImplementing, ActorHuman, "inbox.request_changes", via, notes, "")
}

// Retry answers a blocked or failed item and returns it to implementing
// (the answer feeds the next run's prompt, like request-changes).
func (s *Service) Retry(ctx context.Context, itemID, answer, via string) (*Item, error) {
	return s.transition(ctx, itemID, StatusImplementing, ActorHuman, "inbox.retry", via, answer, "")
}

// Reject discards the item from any workflow phase that allows it.
func (s *Service) Reject(ctx context.Context, itemID, notes, via string) (*Item, error) {
	return s.transition(ctx, itemID, StatusDiscarded, ActorHuman, "inbox.reject", via, notes, "")
}

// MarkDone completes the reviewed item (in_review → done).
func (s *Service) MarkDone(ctx context.Context, itemID, via string) (*Item, error) {
	return s.transition(ctx, itemID, StatusDone, ActorHuman, "inbox.mark_done", via, "", "")
}

// --- system transitions (the executor's path; scope-checked, not approver-gated) ---

// SystemTransition moves an item along the machine phases as a side effect
// of a run: queued → planning, planning → awaiting_approval, approved →
// implementing, implementing → in_review / awaiting_approval (amendment) /
// blocked / failed. agentID names the acting agent for the audit record.
func (s *Service) SystemTransition(ctx context.Context, itemID string, to Status, reason, agentID, via string) (*Item, error) {
	return s.transition(ctx, itemID, to, ActorSystem, "inbox.transition", via, reason, agentID)
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
