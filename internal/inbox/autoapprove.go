package inbox

import (
	"context"
	"fmt"
	"strings"

	"github.com/martinsuchenak/skopos/internal/audit"
	"github.com/martinsuchenak/skopos/internal/auth"
	"github.com/martinsuchenak/skopos/internal/integrations"
	"github.com/martinsuchenak/skopos/internal/plans"
	"github.com/martinsuchenak/skopos/internal/qa"
)

// A plan approved automatically. An approver switches an item's
// auto_approve_plan on; when its plan run succeeds, the server approves the
// plan in their stead unless something holds it for them: a question from the
// planner, several plans to choose from, or a re-plan they asked for. The
// branch still comes to them at sign-off.

// ActionAutoApproveHeld is the audit action of a plan the server did not
// approve although the item asked for it; its notes say why.
const ActionAutoApproveHeld = "inbox.auto_approve_held"

type gateKey int

const (
	quietGateKey gateKey = iota
	heldReasonKey
	nothingToDoKey
)

// NothingToDoReason is why the planner found nothing to do, when the item
// moving (closed, or at the gate) is moving for that reason ("" = not).
func NothingToDoReason(ctx context.Context) string {
	v, _ := ctx.Value(nothingToDoKey).(string)
	return v
}

// ActionNothingToDo is the audit action of an item the server closed because
// the planner found nothing to do.
const ActionNothingToDo = "inbox.nothing_to_do"

// NothingToDo handles a plan round whose planner found nothing to do (a
// routine update with nothing to update). An item whose plan the server may
// approve is closed as done, with the reason; any other waits at the plan
// gate with the reason, for the person to discard or send back. open opens
// the gate. Internal caller (the run chain).
func (s *Service) NothingToDo(ctx context.Context, itemID, reason string, open func(context.Context)) {
	reason = firstLineOf(reason)
	if reason == "" {
		reason = "the planner found nothing to do"
	}
	nctx := context.WithValue(ctx, nothingToDoKey, reason)
	item, err := s.store.GetItem(ctx, strings.TrimSpace(itemID))
	if err != nil || !item.AutoApprovePlan || item.Status != StatusPlanning {
		open(nctx)
		return
	}
	sys := auth.WithPrincipal(context.WithoutCancel(nctx), nil)
	if _, err := s.transition(sys, item.ID, StatusDone, ActorSystem, ActionNothingToDo, audit.ViaSystem, reason, ""); err != nil {
		s.recordHeld(ctx, item.ID, "closing it failed: "+firstLineOf(err.Error()))
		open(nctx)
	}
}

// quietGate reports a plan gate the server approves at once: it is neither
// announced nor sent to other systems as waiting.
func quietGate(ctx context.Context) bool {
	v, _ := ctx.Value(quietGateKey).(bool)
	return v
}

// AutoApproveHeld is why the plan gate being announced was not approved
// automatically ("" = it was not asked for): the gate message says it.
func AutoApproveHeld(ctx context.Context) string {
	v, _ := ctx.Value(heldReasonKey).(string)
	return v
}

// PlanGate opens the plan gate of an item whose plan run succeeded: open
// moves the item to awaiting_approval. With the item's auto_approve_plan on
// and nothing holding it, the gate opens quietly and the plan is approved
// at once; otherwise the gate opens as always, and a held approval is
// recorded with its reason. Internal caller (the run chain).
func (s *Service) PlanGate(ctx context.Context, itemID string, open func(context.Context)) {
	item, err := s.store.GetItem(ctx, strings.TrimSpace(itemID))
	if err != nil || !item.AutoApprovePlan {
		open(ctx)
		return
	}
	if reason := s.autoApproveHold(ctx, item); reason != "" {
		s.recordHeld(ctx, item.ID, reason)
		open(context.WithValue(ctx, heldReasonKey, reason))
		return
	}
	open(context.WithValue(ctx, quietGateKey, true))
	if err := s.autoApprove(ctx, item); err != nil {
		reason := "the approval failed: " + firstLineOf(err.Error())
		s.recordHeld(ctx, item.ID, reason)
		s.announceGate(context.WithValue(ctx, heldReasonKey, reason), item.ID)
	}
}

// HoldPlanGate records why a plan gate opened elsewhere (a round of several
// plans) was not approved automatically, for an item that asked for it.
// The reason goes with the gate's announcement through the context open
// receives.
func (s *Service) HoldPlanGate(ctx context.Context, itemID, reason string, open func(context.Context)) {
	item, err := s.store.GetItem(ctx, strings.TrimSpace(itemID))
	if err != nil || !item.AutoApprovePlan {
		open(ctx)
		return
	}
	s.recordHeld(ctx, item.ID, reason)
	open(context.WithValue(ctx, heldReasonKey, reason))
}

// autoApproveHold is what keeps the plan for the person ("" = nothing).
func (s *Service) autoApproveHold(ctx context.Context, item *Item) string {
	if n, err := s.openQuestions(ctx, item.ID); err != nil {
		return "the planner's questions could not be read"
	} else if n > 0 {
		return fmt.Sprintf("the planner asked %s", plural(n, "question"))
	}
	if s.plans != nil && item.PlanID != "" {
		if rev, err := s.plans.LatestRevision(ctx, item.PlanID); err == nil && rev.GroupID != "" && rev.CandidateState == plans.CandidateOpen {
			return "the round has several plans to choose from"
		}
	}
	replan, known := s.replanAsked(ctx, item.ID)
	switch {
	case !known:
		return "the item's history could not be read"
	case replan:
		return "you asked for this plan again"
	}
	return ""
}

// openQuestions counts the questions on record that wait for an answer.
func (s *Service) openQuestions(ctx context.Context, itemID string) (int, error) {
	if s.questions == nil {
		return 0, nil
	}
	rec, err := s.questions.Record(ctx, itemID)
	if err != nil || rec == nil {
		return 0, err
	}
	n := 0
	for _, set := range rec.Sets {
		for _, q := range set.Questions {
			if q.State == qa.StateOpen {
				n++
			}
		}
	}
	return n, nil
}

// replanAsked reports whether the newest of a person's actions that start a
// plan was a change request: the plan answers it, and they asked to see it
// again. known is false when the history could not be read.
func (s *Service) replanAsked(ctx context.Context, itemID string) (replan, known bool) {
	if s.auditSink == nil {
		return false, false
	}
	planStarts := []string{"inbox.request_changes", "inbox.queue", "inbox.review", "inbox.retry", "inbox.approve"}
	var entries []audit.Entry
	var err error
	// Only the actions that start a plan, when the audit log can filter:
	// an item's full timeline holds every edit and transition.
	if lister, ok := s.auditSink.(interface {
		List(ctx context.Context, f audit.ListFilter) ([]audit.Entry, error)
	}); ok {
		ws, werr := s.store.ItemWorkspace(ctx, itemID)
		if werr != nil {
			return false, false
		}
		entries, err = lister.List(ctx, audit.ListFilter{WorkspaceID: ws, EntityType: auditEntityItem, EntityID: itemID, Actions: planStarts, Limit: 1})
	} else {
		entries, err = s.auditSink.Timeline(ctx, auditEntityItem, itemID) // newest first
	}
	if err != nil {
		return false, false
	}
	for _, e := range entries {
		switch e.Action {
		case "inbox.request_changes":
			return true, true
		case "inbox.queue", "inbox.review", "inbox.retry", "inbox.approve":
			return false, true
		}
	}
	return false, false
}

// autoApprove approves the item's newest plan revision as the server, in
// the name of the person who switched the setting on.
func (s *Service) autoApprove(ctx context.Context, item *Item) error {
	actx := auth.WithPrincipal(context.WithoutCancel(ctx), &auth.Principal{
		Root: true, Approver: true, KeyID: "auto-approve", Name: "auto", AutoFor: item.AutoApproveBy,
	})
	_, err := s.Approve(actx, item.ID, ApproveInput{Via: audit.ViaAuto})
	return err
}

// recordHeld writes the held approval to the item's audit trail, as the
// server.
func (s *Service) recordHeld(ctx context.Context, itemID, reason string) {
	s.auditItem(auth.WithPrincipal(context.WithoutCancel(ctx), nil), itemID, ActionAutoApproveHeld, audit.ViaSystem, reason, "")
}

// announceGate says that an item waits at the plan gate after all: the gate
// opened quietly and the automatic approval failed. It sends what the
// transition kept back: the notification and the waiting events.
func (s *Service) announceGate(ctx context.Context, itemID string) {
	item, err := s.store.GetItem(ctx, itemID)
	if err != nil || item.Status != StatusAwaitingApproval {
		return
	}
	if s.events != nil {
		_ = s.events.Emit(ctx, s.store.EventDB(),
			integrations.Event{Type: integrations.TypePlanReady, ItemID: item.ID, Data: map[string]any{"plan_id": item.PlanID}},
			integrations.Event{Type: integrations.TypeItemNeedsYou, ItemID: item.ID, Data: map[string]any{"gate": "plan_approval"}})
	}
	if s.itemNotifier != nil {
		s.itemNotifier.NotifyItem(ctx, item.ID, StatusPlanning, StatusAwaitingApproval)
	}
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

func firstLineOf(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	return s
}

// nothingToDoMark opens the enrichment a planner that found nothing to do
// writes.
const nothingToDoMark = "**Nothing to do.**"

// newestRoundNothingToDo reports that the item's newest planning round
// found nothing to do (NewestRoundNothingToDo).
func newestRoundNothingToDo(content string) bool {
	return NewestRoundNothingToDo(content) != ""
}

// NewestRoundNothingToDo is why the item's newest planning round found
// nothing to do, read from the item's content: every plan section of the
// round is the planner's "nothing to do" and none names a revision. ""
// when it did not — in a panel round where one planner found nothing to do
// and another wrote a plan, the plan waits for approval.
func NewestRoundNothingToDo(content string) string {
	v, err := PlansOf("", "", content, 0)
	if err != nil || len(v.Plans) == 0 {
		return ""
	}
	why := ""
	for _, p := range v.Plans {
		md := strings.TrimSpace(p.Markdown)
		if p.RevisionID != "" || !strings.HasPrefix(md, nothingToDoMark) {
			return ""
		}
		if why == "" {
			why = firstLineOf(strings.TrimPrefix(md, nothingToDoMark))
		}
	}
	if why == "" {
		why = "the planner found nothing to do"
	}
	return why
}
