package inbox

import (
	"context"
	"errors"
	"testing"
)

// Review fix 1: the manual-path actions refuse workflow items — an agent
// key (which holds the MCP tools for exactly these) cannot bypass the gates.
func TestManualPathRefusesWorkflowItems(t *testing.T) {
	svc, plansSvc, _ := fullStack(t)
	root := context.Background()

	// Walk an item through the post-plan statuses; at each, try all three
	// manual actions as a scoped non-approver key.
	cur := mustReadyForApproval(t, svc, plansSvc, "walker")
	for _, to := range []Status{StatusApproved, StatusImplementing, StatusInReview} {
		var err error
		if to == StatusApproved {
			_, err = svc.Approve(approverCtx(), cur.ID, ApproveInput{})
		} else {
			_, err = svc.SystemTransition(root, cur.ID, to, "", "", "")
		}
		if err != nil {
			t.Fatalf("walk to %s: %v", to, err)
		}
		for name, call := range map[string]func() error{
			"complete": func() error { return svc.Complete(scopedCtx(), cur.ID) },
			"discard":  func() error { return svc.Discard(scopedCtx(), cur.ID) },
			"convert":  func() error { _, err := svc.Convert(scopedCtx(), cur.ID, ConvertInput{PlanID: "p"}); return err },
		} {
			err := call()
			// Linked workflow items refuse convert as already-converted —
			// also a refusal, also not a bypass.
			if !errors.Is(err, ErrInvalidInput) && !errors.Is(err, ErrAlreadyConverted) {
				t.Errorf("%s on %s item: expected refusal, got %v", name, to, err)
			}
		}
	}

	// Queued + planning items refuse convert (the escape hatch from the
	// workflow before any plan exists).
	q := mustCreate(t, svc, root, "queued one")
	if _, err := svc.Queue(approverCtx(), q.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Convert(scopedCtx(), q.ID, ConvertInput{PlanID: "p"}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("convert on queued item: %v", err)
	}
	if err := svc.Complete(scopedCtx(), q.ID); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("complete on queued item: %v", err)
	}
}

// Review fix 2: DeletePlan refuses while a linked item runs the workflow.
func TestDeletePlanGuard(t *testing.T) {
	svc, plansSvc, _ := fullStack(t)
	root := context.Background()
	item := mustReadyForApproval(t, svc, plansSvc, "guard")
	if _, err := svc.Approve(approverCtx(), item.ID, ApproveInput{}); err != nil {
		t.Fatal(err)
	}
	if err := plansSvc.DeletePlan(root, item.PlanID); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("delete plan with workflow item: %v", err)
	}
	// Finish the item and deletion passes.
	if _, err := svc.SystemTransition(root, item.ID, StatusImplementing, "", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SystemTransition(root, item.ID, StatusInReview, "", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.MarkDone(approverCtx(), item.ID, "head", ""); err != nil {
		t.Fatal(err)
	}
	if err := plansSvc.DeletePlan(root, item.PlanID); err != nil {
		t.Fatalf("delete after done: %v", err)
	}
}

// Review fix 5: planning blocks, and Retry returns to the phase the item
// was blocked from — planning when no revision is locked.
func TestPlanningBlocksAndRetryReturnsToOrigin(t *testing.T) {
	svc, plansSvc, _ := fullStack(t)
	root := context.Background()

	// Blocked during planning (no plan yet): retry returns to planning.
	item := mustCreate(t, svc, root, "planner blocked")
	if _, err := svc.Queue(approverCtx(), item.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SystemTransition(root, item.ID, StatusPlanning, "", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SystemTransition(root, item.ID, StatusBlocked, "question", "", ""); err != nil {
		t.Fatalf("planning -> blocked: %v", err)
	}
	after, err := svc.Retry(approverCtx(), item.ID, "the answer", "")
	if err != nil || after.Status != StatusPlanning {
		t.Fatalf("retry from planning-block must return to planning: %s err %v", after.Status, err)
	}

	// Blocked during implementing (locked revision): retry returns to
	// implementing.
	gated := mustReadyForApproval(t, svc, plansSvc, "impl blocked")
	if _, err := svc.Approve(approverCtx(), gated.ID, ApproveInput{}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SystemTransition(root, gated.ID, StatusImplementing, "", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SystemTransition(root, gated.ID, StatusBlocked, "question", "", ""); err != nil {
		t.Fatal(err)
	}
	after, err = svc.Retry(approverCtx(), gated.ID, "the answer", "")
	if err != nil || after.Status != StatusImplementing {
		t.Fatalf("retry from implementing-block must return to implementing: %s err %v", after.Status, err)
	}
}

// Review fix 6: request-changes picks its target by status — the in_review
// leg works over the same call.
func TestRequestChangesPicksTarget(t *testing.T) {
	svc, plansSvc, _ := fullStack(t)
	root := context.Background()
	item := mustReadyForApproval(t, svc, plansSvc, "rc")

	// From the gate: back to planning.
	if _, err := svc.RequestChanges(approverCtx(), item.ID, "tighten", ""); err != nil {
		t.Fatalf("request changes from awaiting_approval: %v", err)
	}
	if got, _ := svc.GetItem(root, item.ID); got.Status != StatusPlanning {
		t.Fatalf("target wrong: %s", got.Status)
	}

	// Walk back to the gate, approve, then on to review.
	if _, err := svc.SystemTransition(root, item.ID, StatusAwaitingApproval, "", "", ""); err != nil {
		t.Fatalf("walk to awaiting_approval: %v", err)
	}
	if _, err := svc.Approve(approverCtx(), item.ID, ApproveInput{}); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if _, err := svc.SystemTransition(root, item.ID, StatusImplementing, "", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SystemTransition(root, item.ID, StatusInReview, "", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RequestChanges(approverCtx(), item.ID, "rename", ""); err != nil {
		t.Fatalf("request changes from in_review: %v", err)
	}
	if got, _ := svc.GetItem(root, item.ID); got.Status != StatusImplementing {
		t.Fatalf("in_review target wrong: %s", got.Status)
	}
}

// Review fix 11: queued items can be rejected; claimed (in_progress) items
// can be queued directly.
func TestQueueAndRejectEdges(t *testing.T) {
	svc, _, _ := fullStack(t)
	root := context.Background()
	item := mustCreate(t, svc, root, "edges")

	if err := svc.Discard(root, item.ID); err != nil {
		t.Fatal(err) // fresh item: manual discard fine (not a workflow item)
	}
	q := mustCreate(t, svc, root, "queued reject")
	if _, err := svc.Queue(approverCtx(), q.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Reject(approverCtx(), q.ID, "", ""); err != nil {
		t.Fatalf("reject from queued: %v", err)
	}

	// in_progress → queued: the queue action releases the claim.
	c := mustCreate(t, svc, root, "claimed")
	if _, err := svc.Claim(root, c.ID, "agent-1"); err != nil {
		t.Fatal(err)
	}
	queued, err := svc.Queue(approverCtx(), c.ID, "")
	if err != nil || queued.Status != StatusQueued {
		t.Fatalf("queue from in_progress: %s err %v", queued.Status, err)
	}
}
